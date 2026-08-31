package store

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Etcd implements KV using the official etcd v3 client.
type Etcd struct {
	client *clientv3.Client
}

func NewEtcd(cluster Cluster, dialTimeout time.Duration) (*Etcd, error) {
	tlsConfig, err := makeTLSConfig(cluster)
	if err != nil {
		return nil, err
	}

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   cluster.Endpoints,
		DialTimeout: dialTimeout,
		Username:    cluster.Username,
		Password:    cluster.Password,
		TLS:         tlsConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("create etcd client: %w", err)
	}
	return &Etcd{client: client}, nil
}

func makeTLSConfig(cluster Cluster) (*tls.Config, error) {
	if cluster.TLSCAFile == "" && cluster.TLSCertFile == "" {
		return nil, nil
	}

	tlsInfo := transport.TLSInfo{
		TrustedCAFile: cluster.TLSCAFile,
		CertFile:      cluster.TLSCertFile,
		KeyFile:       cluster.TLSKeyFile,
	}
	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load etcd TLS configuration: %w", err)
	}
	return tlsConfig, nil
}

func (e *Etcd) Close() error {
	return e.client.Close()
}

func (e *Etcd) Health(ctx context.Context) error {
	_, err := e.client.Get(ctx, "\x00", clientv3.WithFromKey(), clientv3.WithCountOnly(), clientv3.WithLimit(1))
	return err
}

// MemberStatuses asks every configured endpoint for its Raft status. Calls run
// concurrently so one unavailable member cannot delay all healthy members.
func (e *Etcd) MemberStatuses(ctx context.Context, endpoints []string) []MemberStatus {
	statuses := make([]MemberStatus, len(endpoints))
	var wait sync.WaitGroup
	wait.Add(len(endpoints))
	for index, endpoint := range endpoints {
		go func() {
			defer wait.Done()
			member := MemberStatus{Endpoint: endpoint}
			response, err := e.client.Status(ctx, endpoint)
			if err != nil {
				member.Error = err.Error()
				statuses[index] = member
				return
			}
			member.Reachable = true
			member.Healthy = len(response.Errors) == 0
			member.Version = response.Version
			member.LeaderID = response.Leader
			if response.Header != nil {
				member.MemberID = response.Header.MemberId
			}
			if len(response.Errors) > 0 {
				member.Error = strings.Join(response.Errors, "; ")
			}
			statuses[index] = member
		}()
	}
	wait.Wait()
	return statuses
}

func (e *Etcd) List(ctx context.Context, prefix, cursor []byte, limit int64) (Page, error) {
	if limit < 1 {
		return Page{}, errors.New("limit must be positive")
	}
	if len(cursor) > 0 && !bytes.HasPrefix(cursor, prefix) {
		return Page{}, errors.New("cursor is outside the requested prefix")
	}

	start := prefix
	if len(start) == 0 {
		start = []byte{0}
	}
	if len(cursor) > 0 {
		start = cursor
	}

	var rangeEnd string
	if len(prefix) == 0 {
		rangeEnd = "\x00"
	} else {
		rangeEnd = clientv3.GetPrefixRangeEnd(string(prefix))
	}

	response, err := e.client.Get(
		ctx,
		string(start),
		clientv3.WithRange(rangeEnd),
		clientv3.WithLimit(limit+1),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
	)
	if err != nil {
		return Page{}, err
	}

	page := Page{Entries: make([]Entry, 0, min(int64(len(response.Kvs)), limit))}
	for index, item := range response.Kvs {
		if int64(index) == limit {
			page.NextCursor = append([]byte(nil), item.Key...)
			break
		}
		page.Entries = append(page.Entries, entryFromKV(item))
	}
	return page, nil
}

func (e *Etcd) Get(ctx context.Context, key []byte) (Entry, bool, error) {
	response, err := e.client.Get(ctx, string(key), clientv3.WithLimit(1))
	if err != nil {
		return Entry{}, false, err
	}
	if len(response.Kvs) == 0 {
		return Entry{}, false, nil
	}
	return entryFromKV(response.Kvs[0]), true, nil
}

func (e *Etcd) GetAtRevision(ctx context.Context, key []byte, revision int64) (Entry, bool, error) {
	if revision < 1 {
		return Entry{}, false, ErrNoPreviousVersion
	}
	response, err := e.client.Get(ctx, string(key), clientv3.WithRev(revision), clientv3.WithLimit(1))
	if err != nil {
		if rpctypes.Error(err) == rpctypes.ErrCompacted {
			return Entry{}, false, ErrHistoryCompacted
		}
		return Entry{}, false, err
	}
	if len(response.Kvs) == 0 {
		return Entry{}, false, nil
	}
	return entryFromKV(response.Kvs[0]), true, nil
}

func (e *Etcd) Put(ctx context.Context, key, value []byte, expectedModRevision *int64) (int64, error) {
	if expectedModRevision == nil {
		response, err := e.client.Put(ctx, string(key), string(value))
		if err != nil {
			return 0, err
		}
		return response.Header.Revision, nil
	}

	comparison := clientv3.Compare(clientv3.ModRevision(string(key)), "=", *expectedModRevision)
	response, err := e.client.Txn(ctx).
		If(comparison).
		Then(clientv3.OpPut(string(key), string(value))).
		Commit()
	if err != nil {
		return 0, err
	}
	if !response.Succeeded {
		return 0, ErrConflict
	}
	return response.Header.Revision, nil
}

func (e *Etcd) Delete(ctx context.Context, key []byte, expectedModRevision *int64) (int64, bool, error) {
	if expectedModRevision == nil {
		response, err := e.client.Delete(ctx, string(key))
		if err != nil {
			return 0, false, err
		}
		return response.Header.Revision, response.Deleted > 0, nil
	}

	response, err := e.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(string(key)), "=", *expectedModRevision)).
		Then(clientv3.OpDelete(string(key))).
		Commit()
	if err != nil {
		return 0, false, err
	}
	if !response.Succeeded {
		return 0, false, ErrConflict
	}
	return response.Header.Revision, true, nil
}

func entryFromKV(item *mvccpb.KeyValue) Entry {
	return Entry{
		Key:            append([]byte(nil), item.Key...),
		Value:          append([]byte(nil), item.Value...),
		CreateRevision: item.CreateRevision,
		ModRevision:    item.ModRevision,
		Version:        item.Version,
		Lease:          item.Lease,
	}
}
