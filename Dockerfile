FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
ARG GOPROXY=https://proxy.golang.org,direct
RUN GOPROXY=${GOPROXY} go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN mkdir -p /out/data && chown 65532:65532 /out/data && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/etcd-studio ./cmd/etcd-studio

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/etcd-studio /etcd-studio
COPY --from=build --chown=65532:65532 /out/data /data

USER 65532:65532
EXPOSE 8080
ENV LISTEN_ADDR=0.0.0.0:8080
ENV CLUSTERS_FILE=/data/clusters.json
ENV HISTORY_CONFIG_FILE=/data/history-storage.json
ENV HISTORY_FILE=/data/history.jsonl
ENV AUTH_FILE=/data/auth.json
ENTRYPOINT ["/etcd-studio"]
