"use strict";

const state = {
  authStatus: null,
  currentUser: null,
  authProvider: "local",
  appStarted: false,
  authTransitioning: false,
  authSettings: null,
  localUsers: [],
  editingLocalUsername: "",
  clusters: [],
  selectedClusterId: "",
  editingClusterId: "",
  items: [],
  selected: null,
  creating: false,
  prefix: "",
  cursor: "",
  cursorHistory: [],
  nextCursor: "",
  page: 1,
  loadingList: false,
  saving: false,
  valueEncoding: "utf8",
  listRequestID: 0,
  statusRequestID: 0,
  keyRequestID: 0,
  rollbackContext: null,
  historyStorageConfigured: false,
  historyStorageType: "",
  historyRetentionVersions: 100,
  historyStorageStatus: null,
  auditItems: [],
  auditCursor: "",
  auditCursorHistory: [],
  auditNextCursor: "",
  auditPage: 1,
  auditLoading: false,
  auditRequestID: 0,
};

const el = Object.fromEntries(
  [
    "authPage", "authLoading", "passwordChangeForm", "passwordChangeUsername", "passwordRequirements",
    "newPasswordInput", "newPasswordConfirmInput", "passwordConfirmation", "passwordChangeResult", "passwordChangeButton",
    "passwordChangeLogoutButton",
    "loginPanel", "localLoginTab", "ldapLoginTab", "loginForm", "loginProviderCaption",
    "loginUsername", "loginPassword", "loginResult", "loginButton", "appTopbar", "appMain",
    "currentUserAvatar", "currentUserName", "currentUserMeta", "logoutButton", "userPageButton",
    "connectionButton", "statusDot", "statusLabel", "endpointLabel", "prefixForm",
    "clusterSelect", "clusterCount", "manageClustersButton", "historySettingsButton", "auditLogButton",
    "prefixInput", "clearPrefixButton", "refreshButton", "newKeyButton", "emptyNewKeyButton",
    "resultCount", "activePrefix", "keyList", "listState", "previousButton", "nextButton",
    "pageLabel", "emptyEditor", "editorForm", "editorMode", "editorKeyTitle",
    "deleteButton", "keyInput", "copyKeyInlineButton", "keyEncodingLabel", "keyHelp",
    "encodingSelect", "valueInput", "valueStats", "copyValueButton", "createRevision",
    "modRevision", "versionValue", "leaseValue", "saveHint", "cancelButton", "saveButton",
    "deleteDialog", "deleteKeyName", "confirmDeleteButton", "toastRegion", "rollbackButton",
    "rollbackDialog", "rollbackKeyName", "rollbackRevision", "rollbackSource", "rollbackEncoding",
    "rollbackValuePreview", "rollbackVersionList", "rollbackLoadMoreButton", "confirmRollbackButton",
    "clusterDialog", "clusterDialogTitle", "closeClusterDialogButton", "clusterForm",
    "clusterNameInput", "clusterEndpointsInput", "clusterUsernameInput", "clusterPasswordInput",
    "passwordFieldHint", "clearPasswordOption", "clearPasswordCheckbox", "clusterCAInput",
    "clusterCertInput", "clusterKeyInput", "clusterTestResult", "deleteClusterButton",
    "addAnotherClusterButton", "testClusterButton", "saveClusterButton", "deleteClusterDialog",
    "deleteClusterName", "confirmDeleteClusterButton",
    "historyStorageDialog", "historyStorageForm", "storageLocalFields", "storageDatabaseFields",
    "storageLocalFileInput", "storageHostInput", "storagePortInput", "storageDatabaseInput",
    "storageUsernameInput", "storagePasswordInput", "storageTLSModeSelect", "storageTestResult",
    "storageRetentionInput", "testStorageButton", "saveStorageButton",
    "historySettingsDialog", "historySettingsForm", "historyStorageTypeLabel", "historyRetentionInput",
    "historyRetentionResult", "closeHistorySettingsButton", "saveHistorySettingsButton",
    "historyConfiguredAt", "historyLocalFileRow", "historyLocalFile", "historyDatabaseHostRow",
    "historyDatabaseHost", "historyDatabaseNameRow", "historyDatabaseName", "historyDatabaseUsernameRow",
    "historyDatabaseUsername", "historyDatabaseTLSRow", "historyDatabaseTLS", "historyDatabasePasswordRow",
    "historyDatabasePassword", "historyDatabaseSecurityNote",
    "kvIntro", "kvClusterStrip", "kvQueryBar", "kvWorkspace", "auditPage", "auditBackButton",
    "auditRetentionDays", "auditFilterForm", "auditClusterFilter", "auditActionFilter", "auditSearchInput",
    "auditStartDate", "auditEndDate", "auditRefreshButton", "auditTableBody", "auditState",
    "auditPreviousButton", "auditNextButton", "auditPageLabel",
    "usersPage", "usersBackButton", "authSettingsForm", "authConfiguredAt", "ldapSettingsFields",
    "ldapHostInput", "ldapPortInput", "ldapTLSModeSelect", "ldapServerNameInput", "ldapCAFileInput",
    "ldapBaseDNInput", "ldapBindDNInput", "ldapBindPasswordInput", "ldapBindPasswordHint",
    "ldapUserFilterInput", "ldapUserDNTemplateInput", "ldapUsernameAttributeInput",
    "ldapDisplayNameAttributeInput", "ldapAdminUsernamesInput", "ldapTestResult", "testLDAPButton",
    "saveAuthSettingsButton", "addLocalUserButton", "localUsersTableBody", "localUsersEmpty",
    "userEditorDialog", "userEditorTitle", "closeUserEditorButton", "userEditorForm", "localUsernameInput",
    "localDisplayNameInput", "localRoleSelect", "localPasswordInput", "localPasswordHint",
    "localClusterPermissionField", "localClusterPermissionHint", "localClusterPermissions",
    "localUserActiveInput", "userEditorResult", "deleteLocalUserButton", "cancelUserEditorButton",
    "saveLocalUserButton", "deleteLocalUserDialog", "deleteLocalUsername", "confirmDeleteLocalUserButton",
  ].map((id) => [id, document.getElementById(id)]),
);

async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
  });
  let payload = {};
  try {
    payload = await response.json();
  } catch {
    // A useful fallback is produced below.
  }
  if (!response.ok) {
    const error = new Error(payload?.error?.message || `请求失败 (${response.status})`);
    error.code = payload?.error?.code || "request_failed";
    error.status = response.status;
    if ((response.status === 401 || response.status === 428 || error.code === "password_change_required") &&
      !path.startsWith("/api/v1/auth/")) {
      void initializeAuthentication();
    }
    throw error;
  }
  return payload;
}

function showAuthResult(target, message, error = false) {
  target.textContent = message;
  target.hidden = false;
  target.classList.toggle("error", error);
}

function passwordRuleState(password) {
  return {
    length: [...password].length >= 10,
    uppercase: /\p{Lu}/u.test(password),
    lowercase: /\p{Ll}/u.test(password),
    digit: /\p{Nd}/u.test(password),
    special: /[\p{P}\p{S}]/u.test(password),
    noWhitespace: password.length > 0 && !/[\s\p{Cc}]/u.test(password),
    maxBytes: password.length > 0 && new TextEncoder().encode(password).length <= 72,
  };
}

function updatePasswordGuidance() {
  const password = el.newPasswordInput.value;
  const rules = passwordRuleState(password);
  el.passwordRequirements.querySelectorAll("[data-password-rule]").forEach((item) => {
    const met = rules[item.dataset.passwordRule];
    item.classList.toggle("is-met", met);
    item.setAttribute("aria-label", `${item.textContent.trim()}：${met ? "已符合" : "未符合"}`);
  });

  const confirmation = el.newPasswordConfirmInput.value;
  const matches = confirmation.length > 0 && confirmation === password;
  el.passwordConfirmation.classList.toggle("is-met", matches);
  el.passwordConfirmation.classList.toggle("is-mismatch", confirmation.length > 0 && !matches);
  el.passwordConfirmation.textContent = confirmation.length === 0
    ? "请再次输入新密码"
    : matches ? "✓ 两次输入的密码一致" : "两次输入的密码不一致";
  const valid = Object.values(rules).every(Boolean) && matches;
  el.passwordChangeButton.disabled = !valid;
  return valid;
}

function setAuthView(view) {
  el.authPage.hidden = false;
  el.appTopbar.hidden = true;
  el.appMain.hidden = true;
  el.authLoading.hidden = view !== "loading";
  el.passwordChangeForm.hidden = view !== "change-password";
  el.loginPanel.hidden = view !== "login";
  document.title = view === "change-password" ? "设置强密码 · Etcd Studio" : "登录 · Etcd Studio";
}

async function initializeAuthentication() {
  if (state.authTransitioning) return;
  state.authTransitioning = true;
  setAuthView("loading");
  try {
    const status = await api("/api/v1/auth/status");
    state.authStatus = status;
    if (!status.configured) {
      state.currentUser = null;
      configureLoginProviders({ local_enabled: true, ldap_enabled: false });
      setAuthView("login");
      showAuthResult(el.loginResult, "认证初始化未完成，请重启 Etcd Studio 并查看终端输出", true);
      return;
    }
    if (!status.authenticated || !status.user) {
      state.currentUser = null;
      configureLoginProviders(status);
      setAuthView("login");
      requestAnimationFrame(() => el.loginUsername.focus());
      return;
    }
    await enterApplication(status.user);
  } catch (error) {
    setAuthView("login");
    configureLoginProviders({ local_enabled: true, ldap_enabled: false });
    showAuthResult(el.loginResult, `无法读取登录状态：${error.message}`, true);
  } finally {
    state.authTransitioning = false;
  }
}

function configureLoginProviders(status) {
  el.localLoginTab.hidden = !status.local_enabled;
  el.ldapLoginTab.hidden = !status.ldap_enabled;
  const provider = status.local_enabled ? "local" : "ldap";
  selectLoginProvider(provider);
}

function selectLoginProvider(provider) {
  if (provider === "local" && el.localLoginTab.hidden) return;
  if (provider === "ldap" && el.ldapLoginTab.hidden) return;
  state.authProvider = provider;
  const local = provider === "local";
  el.localLoginTab.setAttribute("aria-selected", local ? "true" : "false");
  el.ldapLoginTab.setAttribute("aria-selected", local ? "false" : "true");
  el.loginProviderCaption.textContent = local
    ? "使用 Etcd Studio 本地账户登录"
    : "使用企业 LDAP 目录账户登录";
  el.loginButton.textContent = local ? "本地账户登录" : "LDAP 登录";
  el.loginResult.hidden = true;
}

async function submitPasswordChange(event) {
  event.preventDefault();
  el.passwordChangeResult.hidden = true;
  if (!updatePasswordGuidance()) {
    showAuthResult(el.passwordChangeResult, "请先满足全部密码规则，并确保两次输入一致", true);
    return;
  }
  el.passwordChangeButton.disabled = true;
  el.passwordChangeButton.textContent = "正在保存…";
  try {
    const user = await api("/api/v1/auth/change-password", {
      method: "POST",
      body: JSON.stringify({
        new_password: el.newPasswordInput.value,
      }),
    });
    el.passwordChangeForm.reset();
    state.authStatus = {
      configured: true, local_enabled: true, ldap_enabled: false, authenticated: true, user,
    };
    await enterApplication(user);
    toast("管理员强密码已设置");
  } catch (error) {
    showAuthResult(el.passwordChangeResult, error.message, true);
  } finally {
    el.passwordChangeButton.textContent = "保存新密码并进入系统";
    updatePasswordGuidance();
  }
}

async function submitLogin(event) {
  event.preventDefault();
  el.loginResult.hidden = true;
  el.loginButton.disabled = true;
  const buttonLabel = state.authProvider === "local" ? "本地账户登录" : "LDAP 登录";
  el.loginButton.textContent = "正在登录…";
  try {
    const user = await api("/api/v1/auth/login", {
      method: "POST",
      body: JSON.stringify({
        provider: state.authProvider,
        username: el.loginUsername.value.trim(),
        password: el.loginPassword.value,
      }),
    });
    el.loginPassword.value = "";
    state.authStatus = { ...(state.authStatus || {}), configured: true, authenticated: true, user };
    await enterApplication(user);
  } catch (error) {
    showAuthResult(el.loginResult, error.message, true);
  } finally {
    el.loginButton.disabled = false;
    el.loginButton.textContent = buttonLabel;
  }
}

async function enterApplication(user) {
  state.currentUser = user;
  if (user.must_change_password) {
    el.passwordChangeUsername.textContent = user.username;
    el.passwordChangeResult.hidden = true;
    el.passwordChangeForm.reset();
    updatePasswordGuidance();
    setAuthView("change-password");
    requestAnimationFrame(() => el.newPasswordInput.focus());
    return;
  }
  el.currentUserAvatar.textContent = [...(user.display_name || user.username || "U")][0]?.toUpperCase() || "U";
  el.currentUserName.textContent = user.display_name || user.username;
  const provider = user.provider === "ldap" ? "LDAP" : "本地账户";
  el.currentUserMeta.textContent = `${provider} · ${user.role === "admin" ? "管理员" : "操作员"}`;
  const administrator = user.role === "admin";
  el.userPageButton.hidden = !administrator;
  el.historySettingsButton.hidden = !administrator;
  el.manageClustersButton.hidden = !administrator;
  el.authPage.hidden = true;
  el.appTopbar.hidden = false;
  el.appMain.hidden = false;
  if (user.role !== "admin" && window.location.hash === "#users") window.location.hash = "";
  if (!state.appStarted) {
    state.appStarted = true;
    const configured = await loadHistoryStorage();
    if (configured) await startWorkspace();
  } else {
    showPageFromHash();
  }
}

async function logout() {
  el.logoutButton.disabled = true;
  el.passwordChangeLogoutButton.disabled = true;
  try {
    await api("/api/v1/auth/logout", { method: "POST", body: "{}" });
  } catch (error) {
    if (error.status !== 401) toast(error.message, "error");
  } finally {
    state.currentUser = null;
    state.authSettings = null;
    state.localUsers = [];
    state.appStarted = false;
    el.logoutButton.disabled = false;
    el.passwordChangeLogoutButton.disabled = false;
    window.location.hash = "";
    await initializeAuthentication();
  }
}

async function loadClusters(preferredID = "") {
  try {
    const response = await api("/api/v1/clusters");
    state.clusters = response.items || [];
    const candidate = preferredID || state.selectedClusterId;
    const selected = state.clusters.find((cluster) => cluster.id === candidate) || state.clusters[0] || null;
    state.selectedClusterId = selected?.id || "";
    renderClusterSelect();
    setWorkspaceEnabled(Boolean(selected));
    closeEditor();
    if (selected) {
      checkConnection();
      loadKeys({ reset: true });
    } else {
      state.items = [];
      renderKeyList();
      updatePagination();
      showListState("empty", state.currentUser?.role === "admin"
        ? "请先在集群管理中添加一个 etcd 集群"
        : "暂无可用集群，请联系管理员完成配置");
      checkConnection();
    }
    return selected;
  } catch (error) {
    toast(error.message, "error");
    setWorkspaceEnabled(false);
    return null;
  }
}

async function loadHistoryStorage() {
  try {
    const status = await api("/api/v1/history-storage");
    applyHistoryStorageStatus(status);
    if (!state.historyStorageConfigured) {
      if (state.currentUser?.role === "admin") {
        openHistoryStorageDialog(status.default_local_file || "./data/history.jsonl");
      } else {
        toast("历史存储尚未初始化，请联系管理员", "error");
      }
    }
    return state.historyStorageConfigured;
  } catch (error) {
    toast(error.message, "error");
    setWorkspaceEnabled(false);
    return false;
  }
}

function applyHistoryStorageStatus(status) {
  state.historyStorageStatus = status;
  state.historyStorageConfigured = Boolean(status.configured);
  state.historyStorageType = status.type || "";
  state.historyRetentionVersions = Number(status.retention_versions ?? 100);
}

function openHistoryStorageDialog(defaultLocalFile) {
  const localOption = el.historyStorageForm.querySelector('input[name="historyStorageType"][value="local"]');
  localOption.checked = true;
  el.storageLocalFileInput.value = defaultLocalFile;
  el.storageHostInput.value = "";
  el.storageDatabaseInput.value = "";
  el.storageUsernameInput.value = "";
  el.storagePasswordInput.value = "";
  el.storageRetentionInput.value = String(state.historyRetentionVersions);
  el.storageTestResult.hidden = true;
  updateHistoryStorageFields();
  if (!el.historyStorageDialog.open) el.historyStorageDialog.showModal();
}

function selectedHistoryStorageType() {
  return el.historyStorageForm.querySelector('input[name="historyStorageType"]:checked')?.value || "local";
}

function updateHistoryStorageFields() {
  const type = selectedHistoryStorageType();
  const local = type === "local";
  el.storageLocalFields.hidden = !local;
  el.storageDatabaseFields.hidden = local;
  if (local) return;
  const previousPort = Number(el.storagePortInput.value || 0);
  if (!previousPort || previousPort === 5432 || previousPort === 3306) {
    el.storagePortInput.value = type === "postgres" ? "5432" : "3306";
  }
  const modes = type === "postgres"
    ? [["require", "Require（推荐）"], ["verify-full", "Verify Full"], ["verify-ca", "Verify CA"], ["prefer", "Prefer"], ["disable", "Disable"]]
    : [["preferred", "Preferred（推荐）"], ["require", "Require"], ["skip-verify", "Skip Verify"], ["disable", "Disable"]];
  el.storageTLSModeSelect.replaceChildren(...modes.map(([value, label]) => {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = label;
    return option;
  }));
}

function collectHistoryStorageRequest() {
  const type = selectedHistoryStorageType();
  const retentionVersions = Number(el.storageRetentionInput.value);
  if (type === "local") {
    return { type, local_file: el.storageLocalFileInput.value.trim(), retention_versions: retentionVersions };
  }
  return {
    type,
    host: el.storageHostInput.value.trim(),
    port: Number(el.storagePortInput.value),
    database: el.storageDatabaseInput.value.trim(),
    username: el.storageUsernameInput.value.trim(),
    password: el.storagePasswordInput.value,
    tls_mode: el.storageTLSModeSelect.value,
    retention_versions: retentionVersions,
  };
}

function setHistoryStorageLoading(loading, action) {
  el.testStorageButton.disabled = loading;
  el.saveStorageButton.disabled = loading;
  if (action === "test") el.testStorageButton.textContent = loading ? "测试中…" : "测试连接";
  if (action === "save") el.saveStorageButton.textContent = loading ? "保存中…" : "保存并继续";
}

function showHistoryStorageResult(message, error = false) {
  el.storageTestResult.textContent = message;
  el.storageTestResult.hidden = false;
  el.storageTestResult.classList.toggle("error", error);
}

async function testHistoryStorageConnection() {
  setHistoryStorageLoading(true, "test");
  el.storageTestResult.hidden = true;
  try {
    const result = await api("/api/v1/history-storage/test", {
      method: "POST",
      body: JSON.stringify(collectHistoryStorageRequest()),
    });
    showHistoryStorageResult(result.message, !result.connected);
  } catch (error) {
    showHistoryStorageResult(error.message, true);
  } finally {
    setHistoryStorageLoading(false, "test");
  }
}

async function saveHistoryStorage(event) {
  event.preventDefault();
  setHistoryStorageLoading(true, "save");
  el.storageTestResult.hidden = true;
  try {
    const request = collectHistoryStorageRequest();
    const status = await api("/api/v1/history-storage", {
      method: "PUT",
      body: JSON.stringify(request),
    });
    applyHistoryStorageStatus(status);
    el.historyStorageDialog.close();
    toast("历史存储配置已保存");
    await startWorkspace();
  } catch (error) {
    showHistoryStorageResult(error.message, true);
  } finally {
    setHistoryStorageLoading(false, "save");
  }
}

function formatHistoryStorageType(type = state.historyStorageType) {
  if (type === "postgres") return "PostgreSQL";
  if (type === "mysql") return "MySQL";
  return "本地文件";
}

function openHistorySettings() {
  const status = state.historyStorageStatus || {};
  const database = status.type === "postgres" || status.type === "mysql";
  el.historyStorageTypeLabel.textContent = formatHistoryStorageType(status.type);
  el.historyConfiguredAt.textContent = formatConfiguredAt(status.configured_at);
  el.historyLocalFileRow.hidden = database;
  el.historyLocalFile.textContent = status.local_file || "—";
  for (const row of [
    el.historyDatabaseHostRow, el.historyDatabaseNameRow, el.historyDatabaseUsernameRow,
    el.historyDatabaseTLSRow, el.historyDatabasePasswordRow,
  ]) row.hidden = !database;
  el.historyDatabaseSecurityNote.hidden = !database;
  el.historyDatabaseHost.textContent = database ? `${status.host || "—"}:${status.port || "—"}` : "—";
  el.historyDatabaseName.textContent = status.database || "—";
  el.historyDatabaseUsername.textContent = status.username || "—";
  el.historyDatabaseTLS.textContent = status.tls_mode || "—";
  el.historyDatabasePassword.textContent = status.password_configured ? "已配置（不回显）" : "未配置";
  el.historyRetentionInput.value = String(state.historyRetentionVersions);
  el.historyRetentionResult.hidden = true;
  el.saveHistorySettingsButton.disabled = false;
  el.saveHistorySettingsButton.textContent = "保存策略";
  if (!el.historySettingsDialog.open) el.historySettingsDialog.showModal();
}

function formatConfiguredAt(value) {
  if (!value) return "—";
  const configuredAt = new Date(value);
  if (Number.isNaN(configuredAt.getTime())) return "—";
  return configuredAt.toLocaleString("zh-CN", { hour12: false });
}

async function saveHistorySettings(event) {
  event.preventDefault();
  const retentionVersions = Number(el.historyRetentionInput.value);
  if (!Number.isInteger(retentionVersions) || retentionVersions < 0 || retentionVersions > 10000) {
    el.historyRetentionResult.textContent = "保留版本数必须是 0 到 10000 之间的整数";
    el.historyRetentionResult.hidden = false;
    el.historyRetentionResult.classList.add("error");
    return;
  }
  el.saveHistorySettingsButton.disabled = true;
  el.saveHistorySettingsButton.textContent = "保存并清理中…";
  el.historyRetentionResult.hidden = true;
  try {
    const status = await api("/api/v1/history-storage/retention", {
      method: "PATCH",
      body: JSON.stringify({ retention_versions: retentionVersions }),
    });
    applyHistoryStorageStatus(status);
    el.historySettingsDialog.close();
    toast(retentionVersions === 0 ? "已设置为保留全部历史版本" : `每个 Key 将保留最近 ${formatNumber(retentionVersions)} 个版本`);
  } catch (error) {
    el.historyRetentionResult.textContent = error.message;
    el.historyRetentionResult.hidden = false;
    el.historyRetentionResult.classList.add("error");
  } finally {
    el.saveHistorySettingsButton.disabled = false;
    el.saveHistorySettingsButton.textContent = "保存策略";
  }
}

async function startWorkspace() {
  const cluster = await loadClusters();
  if (!cluster && state.currentUser?.role === "admin" && !["#audit", "#users"].includes(window.location.hash)) {
    openClusterDialog();
  }
  showPageFromHash();
}

function showPageFromHash() {
  const audit = window.location.hash === "#audit";
  const users = window.location.hash === "#users";
  if (users && state.currentUser?.role !== "admin") {
    window.location.hash = "";
    return;
  }
  const secondaryPage = audit || users;
  for (const section of [el.kvIntro, el.kvClusterStrip, el.kvQueryBar, el.kvWorkspace]) section.hidden = secondaryPage;
  el.auditPage.hidden = !audit;
  el.usersPage.hidden = !users;
  document.title = audit ? "审计日志 · Etcd Studio" : (users ? "用户与认证 · Etcd Studio" : "Etcd Studio");
  if (audit) {
    renderAuditClusterFilter();
    loadAudit({ reset: true });
  }
  if (users) loadUsersPage();
}

function openAuditPage() {
  if (window.location.hash === "#audit") {
    showPageFromHash();
    return;
  }
  window.location.hash = "audit";
}

function closeAuditPage() {
  if (window.location.hash === "#audit") {
    window.location.hash = "";
    return;
  }
  showPageFromHash();
}

function openUsersPage() {
  if (state.currentUser?.role !== "admin") return;
  if (window.location.hash === "#users") {
    showPageFromHash();
    return;
  }
  window.location.hash = "users";
}

function closeUsersPage() {
  if (window.location.hash === "#users") {
    window.location.hash = "";
    return;
  }
  showPageFromHash();
}

async function loadUsersPage() {
  if (state.currentUser?.role !== "admin") return;
  el.saveAuthSettingsButton.disabled = true;
  el.addLocalUserButton.disabled = true;
  try {
    const [settings, users] = await Promise.all([
      api("/api/v1/auth/settings"),
      api("/api/v1/users"),
    ]);
    state.authSettings = settings;
    state.localUsers = users.items || [];
    populateAuthSettings(settings);
    renderLocalUsers();
  } catch (error) {
    if (error.status !== 401) toast(error.message, "error");
  } finally {
    el.saveAuthSettingsButton.disabled = false;
    el.addLocalUserButton.disabled = false;
  }
}

function selectedAuthMode() {
  return el.authSettingsForm.querySelector('input[name="authMode"]:checked')?.value || "local";
}

function updateLDAPSettingsVisibility() {
  const enabled = selectedAuthMode() !== "local";
  el.ldapSettingsFields.hidden = !enabled;
  el.testLDAPButton.hidden = !enabled;
}

function populateAuthSettings(settings) {
  const modeInput = el.authSettingsForm.querySelector(`input[name="authMode"][value="${settings.mode || "local"}"]`);
  if (modeInput) modeInput.checked = true;
  const ldap = settings.ldap || {};
  el.authConfiguredAt.textContent = `初始化于 ${formatConfiguredAt(settings.configured_at)}`;
  el.ldapHostInput.value = ldap.host || "";
  el.ldapPortInput.value = String(ldap.port || (ldap.tls_mode === "ldaps" ? 636 : 389));
  el.ldapTLSModeSelect.value = ldap.tls_mode || "starttls";
  el.ldapServerNameInput.value = ldap.server_name || "";
  el.ldapCAFileInput.value = ldap.ca_file || "";
  el.ldapBaseDNInput.value = ldap.base_dn || "";
  el.ldapBindDNInput.value = ldap.bind_dn || "";
  el.ldapBindPasswordInput.value = "";
  el.ldapBindPasswordHint.textContent = ldap.bind_password_configured ? "已配置；留空保持不变" : "可选";
  el.ldapUserFilterInput.value = ldap.user_filter || "(uid={{username}})";
  el.ldapUserDNTemplateInput.value = ldap.user_dn_template || "";
  el.ldapUsernameAttributeInput.value = ldap.username_attribute || "uid";
  el.ldapDisplayNameAttributeInput.value = ldap.display_name_attribute || "cn";
  el.ldapAdminUsernamesInput.value = (ldap.admin_usernames || []).join("\n");
  el.ldapTestResult.hidden = true;
  updateLDAPSettingsVisibility();
}

function collectLDAPSettings() {
  const bindDN = el.ldapBindDNInput.value.trim();
  const ldap = {
    host: el.ldapHostInput.value.trim(),
    port: Number(el.ldapPortInput.value),
    tls_mode: el.ldapTLSModeSelect.value,
    server_name: el.ldapServerNameInput.value.trim(),
    ca_file: el.ldapCAFileInput.value.trim(),
    base_dn: el.ldapBaseDNInput.value.trim(),
    bind_dn: bindDN,
    user_filter: el.ldapUserFilterInput.value.trim(),
    user_dn_template: el.ldapUserDNTemplateInput.value.trim(),
    username_attribute: el.ldapUsernameAttributeInput.value.trim(),
    display_name_attribute: el.ldapDisplayNameAttributeInput.value.trim(),
    admin_usernames: el.ldapAdminUsernamesInput.value.split(/\r?\n|,/).map((value) => value.trim()).filter(Boolean),
  };
  if (el.ldapBindPasswordInput.value !== "" || bindDN === "") {
    ldap.bind_password = el.ldapBindPasswordInput.value;
  }
  return ldap;
}

function setLDAPResult(message, error = false) {
  el.ldapTestResult.textContent = message;
  el.ldapTestResult.hidden = false;
  el.ldapTestResult.classList.toggle("error", error);
}

async function testLDAPConnection() {
  el.testLDAPButton.disabled = true;
  el.testLDAPButton.textContent = "测试中…";
  el.ldapTestResult.hidden = true;
  try {
    const result = await api("/api/v1/auth/ldap/test", {
      method: "POST",
      body: JSON.stringify(collectLDAPSettings()),
    });
    setLDAPResult(result.message, !result.connected);
  } catch (error) {
    setLDAPResult(error.message, true);
  } finally {
    el.testLDAPButton.disabled = false;
    el.testLDAPButton.textContent = "测试 LDAP";
  }
}

async function saveAuthSettings(event) {
  event.preventDefault();
  el.saveAuthSettingsButton.disabled = true;
  el.saveAuthSettingsButton.textContent = "保存中…";
  el.ldapTestResult.hidden = true;
  try {
    const settings = await api("/api/v1/auth/settings", {
      method: "PUT",
      body: JSON.stringify({ mode: selectedAuthMode(), ldap: collectLDAPSettings() }),
    });
    state.authSettings = settings;
    populateAuthSettings(settings);
    toast("登录设置已保存");
    await initializeAuthentication();
  } catch (error) {
    setLDAPResult(error.message, true);
  } finally {
    el.saveAuthSettingsButton.disabled = false;
    el.saveAuthSettingsButton.textContent = "保存登录设置";
  }
}

function renderLocalUsers() {
  el.localUsersEmpty.hidden = state.localUsers.length > 0;
  const fragment = document.createDocumentFragment();
  for (const user of state.localUsers) {
    const row = document.createElement("tr");
    const identity = document.createElement("td");
    identity.className = "user-identity";
    const name = document.createElement("strong");
    name.textContent = user.display_name || user.username;
    const username = document.createElement("small");
    username.textContent = user.username;
    identity.append(name, username);

    const role = document.createElement("td");
    const roleBadge = document.createElement("span");
    roleBadge.className = `user-role-badge ${user.role}`;
    roleBadge.textContent = user.role === "admin" ? "管理员" : "操作员";
    role.append(roleBadge);

    const clusterAccess = document.createElement("td");
    clusterAccess.className = "user-cluster-access";
    const clusterNames = (user.cluster_ids || []).map((clusterID) =>
      state.clusters.find((cluster) => cluster.id === clusterID)?.name || clusterID);
    clusterAccess.textContent = user.role === "admin"
      ? "全部集群"
      : clusterNames.length ? clusterNames.join("、") : "未分配";
    clusterAccess.classList.toggle("empty", user.role !== "admin" && clusterNames.length === 0);
    clusterAccess.title = clusterAccess.textContent;

    const active = document.createElement("td");
    const activeBadge = document.createElement("span");
    activeBadge.className = `user-status-badge ${user.active ? "active" : "inactive"}`;
    activeBadge.textContent = user.active ? "已启用" : "已停用";
    active.append(activeBadge);

    const updated = document.createElement("td");
    updated.textContent = formatConfiguredAt(user.updated_at);

    const actions = document.createElement("td");
    const edit = document.createElement("button");
    edit.type = "button";
    edit.className = "user-edit-button";
    edit.textContent = "编辑";
    edit.addEventListener("click", () => openUserEditor(user));
    actions.append(edit);
    row.append(identity, role, clusterAccess, active, updated, actions);
    fragment.append(row);
  }
  el.localUsersTableBody.replaceChildren(fragment);
}

function openUserEditor(user = null) {
  state.editingLocalUsername = user?.username || "";
  const editing = Boolean(user);
  const editingSelf = editing && state.currentUser?.provider === "local" &&
    state.currentUser.username.toLowerCase() === user.username.toLowerCase();
  el.userEditorTitle.textContent = editing ? "编辑本地用户" : "添加本地用户";
  el.localUsernameInput.value = user?.username || "";
  el.localUsernameInput.disabled = editing;
  el.localDisplayNameInput.value = user?.display_name || "";
  el.localRoleSelect.value = user?.role || "operator";
  el.localRoleSelect.disabled = editingSelf;
  el.localPasswordInput.value = "";
  el.localPasswordInput.required = !editing;
  el.localPasswordHint.textContent = editing ? "留空保持不变；新密码必须符合强密码规则" : "至少 10 位，含大小写、数字和特殊字符";
  renderLocalUserClusterPermissions(user?.cluster_ids || []);
  el.localUserActiveInput.checked = user?.active ?? true;
  el.localUserActiveInput.disabled = editingSelf;
  el.userEditorResult.hidden = true;
  el.deleteLocalUserButton.hidden = !editing || editingSelf;
  el.saveLocalUserButton.disabled = false;
  if (!el.userEditorDialog.open) el.userEditorDialog.showModal();
  requestAnimationFrame(() => (editing ? el.localDisplayNameInput : el.localUsernameInput).focus());
}

function renderLocalUserClusterPermissions(selectedClusterIDs = []) {
  const selected = new Set(selectedClusterIDs);
  const fragment = document.createDocumentFragment();
  if (!state.clusters.length) {
    const empty = document.createElement("p");
    empty.className = "cluster-permission-empty";
    empty.textContent = "尚未配置集群，可先保存账号，之后再编辑分配。";
    fragment.append(empty);
  } else {
    for (const cluster of state.clusters) {
      const option = document.createElement("label");
      const checkbox = document.createElement("input");
      checkbox.type = "checkbox";
      checkbox.value = cluster.id;
      checkbox.checked = selected.has(cluster.id);
      const name = document.createElement("span");
      name.textContent = cluster.name;
      option.append(checkbox, name);
      fragment.append(option);
    }
  }
  el.localClusterPermissions.replaceChildren(fragment);
  updateLocalUserClusterPermissionVisibility();
}

function updateLocalUserClusterPermissionVisibility() {
  const administrator = el.localRoleSelect.value === "admin";
  el.localClusterPermissions.hidden = administrator;
  el.localClusterPermissionField.classList.toggle("administrator", administrator);
  el.localClusterPermissionHint.textContent = administrator
    ? "管理员自动拥有全部集群权限"
    : "未选择时登录后不显示任何集群";
}

function selectedLocalUserClusterIDs() {
  if (el.localRoleSelect.value === "admin") return [];
  return [...el.localClusterPermissions.querySelectorAll('input[type="checkbox"]:checked')]
    .map((checkbox) => checkbox.value);
}

function showUserEditorResult(message) {
  el.userEditorResult.textContent = message;
  el.userEditorResult.hidden = false;
  el.userEditorResult.classList.add("error");
}

async function saveLocalUser(event) {
  event.preventDefault();
  const editingUsername = state.editingLocalUsername;
  const input = {
    username: el.localUsernameInput.value.trim(),
    display_name: el.localDisplayNameInput.value.trim(),
    role: el.localRoleSelect.value,
    active: el.localUserActiveInput.checked,
    cluster_ids: selectedLocalUserClusterIDs(),
  };
  if (el.localPasswordInput.value) input.password = el.localPasswordInput.value;
  el.saveLocalUserButton.disabled = true;
  el.saveLocalUserButton.textContent = "保存中…";
  el.userEditorResult.hidden = true;
  try {
    await api(editingUsername ? `/api/v1/users/${encodeURIComponent(editingUsername)}` : "/api/v1/users", {
      method: editingUsername ? "PUT" : "POST",
      body: JSON.stringify(input),
    });
    el.userEditorDialog.close();
    toast(editingUsername ? "用户已更新" : "本地用户已创建");
    const editingSelf = editingUsername && state.currentUser?.provider === "local" &&
      state.currentUser.username.toLowerCase() === editingUsername.toLowerCase();
    if (editingSelf) await initializeAuthentication();
    else await loadUsersPage();
  } catch (error) {
    showUserEditorResult(error.message);
  } finally {
    el.saveLocalUserButton.disabled = false;
    el.saveLocalUserButton.textContent = "保存用户";
  }
}

function requestDeleteLocalUser() {
  if (!state.editingLocalUsername) return;
  el.deleteLocalUsername.textContent = state.editingLocalUsername;
  el.deleteLocalUserDialog.showModal();
}

async function deleteLocalUser() {
  const username = state.editingLocalUsername;
  if (!username) return;
  el.confirmDeleteLocalUserButton.disabled = true;
  try {
    await api(`/api/v1/users/${encodeURIComponent(username)}`, { method: "DELETE" });
    if (el.userEditorDialog.open) el.userEditorDialog.close();
    toast("本地用户已删除");
    state.editingLocalUsername = "";
    await loadUsersPage();
  } catch (error) {
    showUserEditorResult(error.message);
  } finally {
    el.confirmDeleteLocalUserButton.disabled = false;
  }
}

function renderAuditClusterFilter() {
  const selected = el.auditClusterFilter.value;
  const administrator = state.currentUser?.role === "admin";
  const options = administrator ? [new Option("全部集群", "")] : [];
  for (const cluster of state.clusters) options.push(new Option(cluster.name, cluster.id));
  if (!administrator && options.length === 0) options.push(new Option("暂无授权集群", ""));
  el.auditClusterFilter.replaceChildren(...options);
  if (state.clusters.some((cluster) => cluster.id === selected)) {
    el.auditClusterFilter.value = selected;
  } else if (!administrator && state.clusters.length > 0) {
    el.auditClusterFilter.value = state.clusters[0].id;
  }
}

async function loadAudit({ reset = false } = {}) {
  if (reset) {
    state.auditCursor = "";
    state.auditCursorHistory = [];
    state.auditNextCursor = "";
    state.auditPage = 1;
  }
  if (state.currentUser?.role !== "admin" && !el.auditClusterFilter.value) {
    state.auditItems = [];
    state.auditNextCursor = "";
    el.auditTableBody.replaceChildren();
    showAuditState("empty", "暂无已授权集群的审计记录");
    updateAuditPagination();
    return;
  }
  const requestID = ++state.auditRequestID;
  const params = new URLSearchParams({ limit: "50" });
  if (el.auditClusterFilter.value) params.set("cluster_id", el.auditClusterFilter.value);
  if (el.auditActionFilter.value) params.set("action", el.auditActionFilter.value);
  if (el.auditSearchInput.value.trim()) params.set("search", el.auditSearchInput.value.trim());
  const dateRange = auditDateRange();
  if (!dateRange) return;
  if (dateRange.from) params.set("from", dateRange.from);
  if (dateRange.to) params.set("to", dateRange.to);
  if (state.auditCursor) params.set("cursor", state.auditCursor);
  state.auditLoading = true;
  showAuditState("loading", "正在读取审计记录…");
  updateAuditPagination();
  try {
    const response = await api(`/api/v1/audit?${params}`);
    if (requestID !== state.auditRequestID) return;
    state.auditItems = response.items || [];
    state.auditNextCursor = response.next_cursor || "";
    el.auditRetentionDays.textContent = String(response.retention_days || 90);
    renderAuditTable();
    updateAuditPagination();
  } catch (error) {
    if (requestID !== state.auditRequestID) return;
    state.auditItems = [];
    state.auditNextCursor = "";
    el.auditTableBody.replaceChildren();
    showAuditState("error", error.message);
    updateAuditPagination();
  } finally {
    if (requestID === state.auditRequestID) state.auditLoading = false;
  }
}

function auditDateRange() {
  const start = el.auditStartDate.value;
  const end = el.auditEndDate.value;
  if (start && end && start > end) {
    toast("结束日期不能早于开始日期", "error");
    return null;
  }
  const range = { from: "", to: "" };
  if (start) range.from = new Date(`${start}T00:00:00`).toISOString();
  if (end) {
    const exclusiveEnd = new Date(`${end}T00:00:00`);
    exclusiveEnd.setDate(exclusiveEnd.getDate() + 1);
    range.to = exclusiveEnd.toISOString();
  }
  return range;
}

function renderAuditTable() {
  if (!state.auditItems.length) {
    el.auditTableBody.replaceChildren();
    showAuditState("empty", "暂无符合条件的审计记录");
    return;
  }
  const fragment = document.createDocumentFragment();
  for (const item of state.auditItems) {
    const row = document.createElement("tr");
    const occurredAt = document.createElement("td");
    occurredAt.className = "audit-time";
    occurredAt.textContent = formatAuditTime(item.occurred_at);

    const actor = document.createElement("td");
    const actorName = document.createElement("span");
    actorName.className = "audit-actor";
    actorName.textContent = item.actor || "未知客户端";
    const actorSource = document.createElement("small");
    actorSource.className = "audit-actor-source";
    actorSource.textContent = auditActorSourceLabel(item.actor_type);
    actor.append(actorName, actorSource);

    const action = document.createElement("td");
    const actionBadge = document.createElement("span");
    actionBadge.className = "audit-action-badge";
    actionBadge.textContent = auditActionLabel(item.action);
    action.append(actionBadge);

    const target = document.createElement("td");
    target.className = "audit-target";
    target.textContent = item.target || "—";

    const cluster = document.createElement("td");
    cluster.textContent = item.cluster_name || "系统";
    if (!item.cluster_name) cluster.className = "audit-cluster-empty";

    const detail = document.createElement("td");
    detail.textContent = item.detail || "操作成功";
    row.append(occurredAt, actor, action, target, cluster, detail);
    fragment.append(row);
  }
  el.auditTableBody.replaceChildren(fragment);
  el.auditState.hidden = true;
}

function showAuditState(type, message) {
  el.auditState.hidden = false;
  const spinner = el.auditState.querySelector(".spinner");
  spinner.hidden = type !== "loading";
  el.auditState.querySelector("p").textContent = message;
}

function updateAuditPagination() {
  el.auditPreviousButton.disabled = state.auditLoading || state.auditCursorHistory.length === 0;
  el.auditNextButton.disabled = state.auditLoading || !state.auditNextCursor;
  el.auditPageLabel.textContent = `第 ${state.auditPage} 页`;
}

function formatAuditTime(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return new Intl.DateTimeFormat("zh-CN", {
    year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false,
  }).format(date);
}

function auditActorSourceLabel(type) {
  if (type === "local") return "本地账户";
  if (type === "ldap") return "LDAP 账户";
  if (type === "authenticated_user") return "认证用户";
  if (type === "client_ip") return "客户端 IP";
  return "身份未知";
}

function auditActionLabel(action) {
  return ({
    "key.create": "新建 Key",
    "key.update": "修改 Key",
    "key.delete": "删除 Key",
    "key.rollback": "回滚 Key",
    "cluster.create": "新增集群",
    "cluster.update": "修改集群",
    "cluster.delete": "删除集群",
    "history_storage.configure": "初始化存储",
    "history_retention.update": "修改历史策略",
    "auth.bootstrap": "初始化管理员",
    "auth.login": "用户登录",
    "auth.logout": "退出登录",
    "auth.password.change": "修改密码",
    "auth.settings.update": "修改认证设置",
    "user.create": "新增用户",
    "user.update": "修改用户",
    "user.delete": "删除用户",
  })[action] || action || "未知操作";
}

function renderClusterSelect() {
  el.clusterSelect.replaceChildren();
  if (state.clusters.length === 0) {
    const option = document.createElement("option");
    option.value = "";
    option.textContent = "尚未配置集群";
    el.clusterSelect.append(option);
  } else {
    for (const cluster of state.clusters) {
      const option = document.createElement("option");
      option.value = cluster.id;
      option.textContent = cluster.name;
      el.clusterSelect.append(option);
    }
  }
  el.clusterSelect.value = state.selectedClusterId;
  el.clusterCount.textContent = `${state.clusters.length} 个集群`;
  renderAuditClusterFilter();
}

function setWorkspaceEnabled(enabled) {
  el.prefixInput.disabled = !enabled;
  el.refreshButton.disabled = !enabled;
  el.newKeyButton.disabled = !enabled;
  el.emptyNewKeyButton.disabled = !enabled;
}

async function activateCluster(clusterID) {
  if (clusterID === state.selectedClusterId) return;
  state.selectedClusterId = clusterID;
  state.selected = null;
  state.creating = false;
  state.items = [];
  closeEditor();
  setWorkspaceEnabled(Boolean(clusterID));
  await Promise.all([checkConnection(), loadKeys({ reset: true })]);
}

function openClusterDialog(cluster = null) {
  state.editingClusterId = cluster?.id || "";
  el.clusterDialogTitle.textContent = cluster ? "编辑 etcd 集群" : "添加 etcd 集群";
  el.clusterNameInput.value = cluster?.name || "";
  el.clusterEndpointsInput.value = cluster?.endpoints?.join("\n") || "";
  el.clusterUsernameInput.value = cluster?.username || "";
  el.clusterPasswordInput.value = "";
  el.clusterPasswordInput.disabled = false;
  el.passwordFieldHint.textContent = cluster?.password_configured ? "留空保持不变" : "可选";
  el.clearPasswordOption.hidden = !cluster?.password_configured;
  el.clearPasswordCheckbox.checked = false;
  el.clusterCAInput.value = cluster?.tls_ca_file || "";
  el.clusterCertInput.value = cluster?.tls_cert_file || "";
  el.clusterKeyInput.value = cluster?.tls_key_file || "";
  el.deleteClusterButton.hidden = !cluster;
  el.addAnotherClusterButton.hidden = !cluster;
  el.clusterTestResult.hidden = true;
  el.clusterTestResult.classList.remove("error");
  const tlsDetails = el.clusterCAInput.closest("details");
  tlsDetails.open = Boolean(cluster?.tls_ca_file || cluster?.tls_cert_file || cluster?.tls_key_file);
  if (!el.clusterDialog.open) el.clusterDialog.showModal();
  requestAnimationFrame(() => el.clusterNameInput.focus());
}

function selectedCluster() {
  return state.clusters.find((cluster) => cluster.id === state.selectedClusterId) || null;
}

function collectClusterRequest() {
  const request = {
    id: state.editingClusterId,
    name: el.clusterNameInput.value.trim(),
    endpoints: el.clusterEndpointsInput.value.split(/[\n,]+/).map((value) => value.trim()).filter(Boolean),
    username: el.clusterUsernameInput.value.trim(),
    clear_password: el.clearPasswordCheckbox.checked,
    tls_ca_file: el.clusterCAInput.value.trim(),
    tls_cert_file: el.clusterCertInput.value.trim(),
    tls_key_file: el.clusterKeyInput.value.trim(),
  };
  if (el.clusterPasswordInput.value !== "") request.password = el.clusterPasswordInput.value;
  return request;
}

async function saveCluster(event) {
  event.preventDefault();
  const request = collectClusterRequest();
  setClusterFormLoading(true, "save");
  try {
    const cluster = await api(
      state.editingClusterId ? `/api/v1/clusters/${encodeURIComponent(state.editingClusterId)}` : "/api/v1/clusters",
      { method: state.editingClusterId ? "PUT" : "POST", body: JSON.stringify(request) },
    );
    toast(state.editingClusterId ? "集群配置已更新" : "集群配置已添加");
    el.clusterDialog.close();
    await loadClusters(cluster.id);
  } catch (error) {
    showClusterTestResult(error.message, true);
  } finally {
    setClusterFormLoading(false, "save");
  }
}

async function testClusterConnection() {
  const request = collectClusterRequest();
  setClusterFormLoading(true, "test");
  el.clusterTestResult.hidden = true;
  try {
    const result = await api("/api/v1/clusters/test", { method: "POST", body: JSON.stringify(request) });
    showClusterTestResult(result.message, !result.connected);
  } catch (error) {
    showClusterTestResult(error.message, true);
  } finally {
    setClusterFormLoading(false, "test");
  }
}

function setClusterFormLoading(loading, action) {
  el.saveClusterButton.disabled = loading;
  el.testClusterButton.disabled = loading;
  if (action === "save") el.saveClusterButton.textContent = loading ? "保存中…" : "保存集群";
  if (action === "test") el.testClusterButton.textContent = loading ? "测试中…" : "测试连接";
}

function showClusterTestResult(message, error = false) {
  el.clusterTestResult.textContent = message;
  el.clusterTestResult.hidden = false;
  el.clusterTestResult.classList.toggle("error", error);
}

async function deleteClusterConfiguration() {
  if (!state.editingClusterId) return;
  el.confirmDeleteClusterButton.disabled = true;
  const deletedID = state.editingClusterId;
  try {
    await api(`/api/v1/clusters/${encodeURIComponent(deletedID)}`, { method: "DELETE" });
    toast("集群配置已删除");
    el.clusterDialog.close();
    const remaining = await loadClusters();
    if (!remaining) openClusterDialog();
  } catch (error) {
    toast(error.message, "error");
  } finally {
    el.confirmDeleteClusterButton.disabled = false;
  }
}

async function checkConnection() {
  const requestID = ++state.statusRequestID;
  const clusterID = state.selectedClusterId;
  if (!state.selectedClusterId) {
    el.statusDot.className = "status-dot checking";
    el.statusLabel.textContent = "未选择集群";
    el.endpointLabel.textContent = "请配置连接";
    el.connectionButton.classList.remove("is-loading");
    return;
  }
  el.connectionButton.classList.add("is-loading");
  el.statusDot.className = "status-dot checking";
  el.statusLabel.textContent = "检测连接";
  try {
    const cluster = state.clusters.find((item) => item.id === clusterID);
    if (cluster) {
      const endpoints = Array.isArray(cluster.endpoints) ? cluster.endpoints : [];
      if (endpoints.length === 1) {
        el.endpointLabel.textContent = endpoints[0];
      } else if (endpoints.length > 1) {
        el.endpointLabel.textContent = `${endpoints.length} 个节点`;
      } else {
        el.endpointLabel.textContent = cluster.name || "etcd";
      }
    }
    const params = new URLSearchParams({ cluster_id: clusterID });
    const status = await api(`/api/v1/status?${params}`);
    if (requestID !== state.statusRequestID || clusterID !== state.selectedClusterId) return;
    const multiMember = Number(status.member_count) > 1;
    if (multiMember) {
      el.endpointLabel.textContent = `Leader · ${status.leader || "未识别"}`;
    } else {
      el.endpointLabel.textContent = status.endpoint || "etcd";
    }
    el.statusLabel.textContent = status.connected ? (status.degraded ? "部分异常" : "已连接") : "连接异常";
    el.statusDot.className = `status-dot ${status.connected ? (status.degraded ? "warning" : "online") : "offline"}`;
    const leaderMessage = multiMember ? `；当前 Leader：${status.leader || "未识别"}` : "";
    el.connectionButton.title = `${status.message || "重新检测连接"}${leaderMessage}`;
  } catch (error) {
    if (requestID !== state.statusRequestID || clusterID !== state.selectedClusterId) return;
    el.statusLabel.textContent = "服务异常";
    el.statusDot.className = "status-dot offline";
    el.connectionButton.title = error.message;
  } finally {
    if (requestID === state.statusRequestID) el.connectionButton.classList.remove("is-loading");
  }
}

async function loadKeys({ reset = false } = {}) {
  const requestID = ++state.listRequestID;
  const clusterID = state.selectedClusterId;
  if (!state.selectedClusterId) {
    state.items = [];
    state.nextCursor = "";
    renderKeyList();
    updatePagination();
    showListState("empty", "请先在集群管理中添加一个 etcd 集群");
    return;
  }
  if (reset) {
    state.cursor = "";
    state.cursorHistory = [];
    state.page = 1;
  }

  state.loadingList = true;
  showListState("loading", "正在读取 etcd…");
  el.refreshButton.classList.add("is-spinning");
  const params = new URLSearchParams({ cluster_id: clusterID, prefix: state.prefix, limit: "50" });
  if (state.cursor) params.set("cursor", state.cursor);

  try {
    const response = await api(`/api/v1/keys?${params}`);
    if (requestID !== state.listRequestID || clusterID !== state.selectedClusterId) return;
    state.items = response.items || [];
    state.nextCursor = response.next_cursor || "";
    renderKeyList();
    updatePagination();
    if (state.items.length === 0) {
      showListState("empty", state.prefix ? "当前前缀下没有 Key" : "etcd 中还没有 Key");
    } else {
      hideListState();
    }
  } catch (error) {
    if (requestID !== state.listRequestID || clusterID !== state.selectedClusterId) return;
    state.items = [];
    state.nextCursor = "";
    renderKeyList();
    updatePagination();
    showListState("error", error.message);
    toast(error.message, "error");
  } finally {
    if (requestID === state.listRequestID) {
      state.loadingList = false;
      el.refreshButton.classList.remove("is-spinning");
    }
  }
}

function renderKeyList() {
  el.keyList.replaceChildren();
  for (const item of state.items) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "key-row";
    button.dataset.keyBase64 = item.key_base64;
    button.setAttribute("role", "option");

    if (state.selected?.key_base64 === item.key_base64 && !state.creating) {
      button.classList.add("selected");
      button.setAttribute("aria-selected", "true");
    } else {
      button.setAttribute("aria-selected", "false");
    }

    const icon = document.createElement("span");
    icon.className = "key-icon";
    icon.innerHTML = '<svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="7" cy="10" r="3.5"/><path d="M10.5 10H17M14.5 10v2M16.5 10v-2"/></svg>';

    const copy = document.createElement("span");
    copy.className = "key-row-copy";
    const name = document.createElement("span");
    name.className = "key-name";
    name.textContent = displayKey(item);
    name.title = displayKey(item);
    const preview = document.createElement("span");
    preview.className = "key-preview";
    if (item.value_is_utf8) {
      preview.textContent = item.value_preview || "(空值)";
    } else {
      preview.textContent = `二进制数据 · ${formatBytes(item.value_bytes)}`;
      preview.classList.add("binary-tag");
    }
    copy.append(name, preview);
    button.append(icon, copy);
    button.addEventListener("click", () => loadKey(item));
    el.keyList.append(button);
  }

  el.resultCount.textContent = `${state.items.length} 项`;
  el.activePrefix.textContent = state.prefix || "全部";
  el.activePrefix.title = state.prefix || "全部 Key";
}

async function loadKey(item) {
  if (!item?.key_base64 || !state.selectedClusterId) return;
  const requestID = ++state.keyRequestID;
  const clusterID = state.selectedClusterId;
  const params = new URLSearchParams({ cluster_id: clusterID, key_base64: item.key_base64 });
  try {
    const detail = await api(`/api/v1/key?${params}`);
    if (requestID !== state.keyRequestID || clusterID !== state.selectedClusterId) return;
    state.selected = detail;
    state.creating = false;
    showEditor(detail);
    renderKeyList();
  } catch (error) {
    if (requestID !== state.keyRequestID || clusterID !== state.selectedClusterId) return;
    toast(error.message, "error");
    if (error.status === 404) loadKeys();
  }
}

function showEditor(detail) {
  el.emptyEditor.hidden = true;
  el.editorForm.hidden = false;
  el.editorMode.textContent = state.creating ? "新建 Key" : "编辑 Key";
  el.editorKeyTitle.textContent = state.creating ? "未命名" : displayKey(detail);

  el.keyInput.disabled = !state.creating;
  el.keyInput.value = state.creating ? "" : displayKey(detail);
  el.keyEncodingLabel.textContent = detail.key_is_utf8 === false ? "Base64（只读）" : "UTF-8";
  el.keyHelp.textContent = state.creating
    ? "建议使用清晰的路径式命名，例如 /services/payment/timeout。"
    : "Key 创建后不可重命名；如需更名，请新建并删除原 Key。";

  state.valueEncoding = detail.value_is_utf8 === false ? "base64" : "utf8";
  el.encodingSelect.value = state.valueEncoding;
  el.valueInput.value = state.valueEncoding === "base64" ? detail.value_base64 || "" : detail.value || "";
  el.deleteButton.hidden = state.creating;
  el.rollbackButton.hidden = state.creating;
  el.rollbackButton.disabled = false;
  el.rollbackButton.title = "选择历史版本回滚";
  el.copyKeyInlineButton.hidden = state.creating;

  el.createRevision.textContent = state.creating ? "保存后生成" : formatNumber(detail.create_revision);
  el.modRevision.textContent = state.creating ? "—" : formatNumber(detail.mod_revision);
  el.versionValue.textContent = state.creating ? "—" : formatNumber(detail.version);
  el.leaseValue.textContent = state.creating ? "无" : (detail.lease ? formatNumber(detail.lease) : "无");
  el.saveHint.textContent = state.creating
    ? "新建时会检查同名 Key，避免意外覆盖已有数据。"
    : "保存时会校验修改版本，避免覆盖其他客户端的更新。";
  updateValueStats();

  if (state.creating) {
    requestAnimationFrame(() => el.keyInput.focus());
  } else {
    requestAnimationFrame(() => el.valueInput.focus());
  }
}

function beginCreate() {
  if (!state.selectedClusterId) {
    openClusterDialog();
    return;
  }
  state.creating = true;
  state.selected = null;
  showEditor({ key_is_utf8: true, value_is_utf8: true, value: "", value_base64: "" });
  renderKeyList();
}

function closeEditor() {
  state.selected = null;
  state.creating = false;
  el.editorForm.hidden = true;
  el.emptyEditor.hidden = false;
  renderKeyList();
}

async function saveKey(event) {
  event?.preventDefault();
  if (state.saving) return;

  const key = el.keyInput.value;
  if (state.creating && key.length === 0) {
    toast("Key 不能为空", "error");
    el.keyInput.focus();
    return;
  }

  const payload = {
    cluster_id: state.selectedClusterId,
    value_encoding: state.valueEncoding,
    expected_mod_revision: state.creating ? 0 : state.selected.mod_revision,
  };
  if (state.creating) payload.key = key;
  else payload.key_base64 = state.selected.key_base64;
  if (state.valueEncoding === "base64") payload.value_base64 = el.valueInput.value.trim();
  else payload.value = el.valueInput.value;

  state.saving = true;
  setSaving(true);
  try {
    await api("/api/v1/key", { method: "PUT", body: JSON.stringify(payload) });
    const savedKeyBase64 = state.creating ? bytesToBase64(new TextEncoder().encode(key)) : state.selected.key_base64;
    toast(state.creating ? "Key 创建成功" : "更改已保存");
    await loadKeys();
    await loadKey({ key_base64: savedKeyBase64 });
  } catch (error) {
    toast(error.message, "error");
    if (error.code === "revision_conflict" && state.selected) {
      el.saveHint.textContent = "检测到版本冲突。请刷新读取最新值，再决定是否修改。";
    }
  } finally {
    state.saving = false;
    setSaving(false);
  }
}

function setSaving(saving) {
  el.saveButton.disabled = saving;
  el.saveButton.classList.toggle("is-loading", saving);
  el.saveButton.lastChild.textContent = saving ? " 保存中…" : " 保存更改";
}

async function deleteSelected() {
  if (!state.selected || state.creating) return;
  const params = new URLSearchParams({
    cluster_id: state.selectedClusterId,
    key_base64: state.selected.key_base64,
    expected_mod_revision: String(state.selected.mod_revision),
  });
  try {
    el.confirmDeleteButton.disabled = true;
    await api(`/api/v1/key?${params}`, { method: "DELETE" });
    toast("Key 已删除");
    closeEditor();
    await loadKeys();
  } catch (error) {
    toast(error.message, "error");
  } finally {
    el.confirmDeleteButton.disabled = false;
  }
}

async function previewRollback() {
  if (!state.selected || state.creating) return;
  const clusterID = state.selectedClusterId;
  const selected = state.selected;
  el.rollbackButton.disabled = true;
  el.rollbackButton.title = "正在读取历史版本…";
  try {
    state.rollbackContext = {
      clusterID,
      keyBase64: selected.key_base64,
      expectedModRevision: selected.mod_revision,
      targetModRevision: null,
      items: [],
      nextBeforeModRevision: 0,
      previewRequestID: 0,
    };
    el.rollbackKeyName.textContent = displayKey(selected);
    el.rollbackRevision.textContent = "—";
    el.rollbackSource.textContent = "—";
    el.rollbackEncoding.textContent = "—";
    el.rollbackValuePreview.textContent = "正在读取历史版本…";
    el.rollbackVersionList.replaceChildren();
    el.rollbackLoadMoreButton.hidden = true;
    el.confirmRollbackButton.disabled = true;
    el.rollbackDialog.returnValue = "";
    el.rollbackDialog.showModal();
    await loadRollbackVersions();
  } catch (error) {
    if (el.rollbackDialog.open) el.rollbackDialog.close();
    toast(error.message, "error");
    if (error.code === "revision_conflict") loadKey(selected);
    if (error.code === "history_compacted" || error.code === "rollback_unavailable") {
      el.rollbackButton.title = error.message;
    }
  } finally {
    el.rollbackButton.disabled = false;
    if (el.rollbackButton.title === "正在读取历史版本…") {
      el.rollbackButton.title = "选择历史版本回滚";
    }
  }
}

async function loadRollbackVersions({ append = false } = {}) {
  const context = state.rollbackContext;
  if (!context) return;
  const params = new URLSearchParams({
    cluster_id: context.clusterID,
    key_base64: context.keyBase64,
    expected_mod_revision: String(context.expectedModRevision),
    limit: "25",
  });
  if (append && context.nextBeforeModRevision) {
    params.set("before_mod_revision", String(context.nextBeforeModRevision));
  }
  el.rollbackLoadMoreButton.disabled = true;
  el.rollbackLoadMoreButton.textContent = append ? "加载中…" : "读取中…";
  const response = await api(`/api/v1/key/rollback-versions?${params}`);
  if (context !== state.rollbackContext) return;
  context.items = append ? context.items.concat(response.items || []) : (response.items || []);
  context.nextBeforeModRevision = Number(response.next_before_mod_revision || 0);
  renderRollbackVersions();
  if (!context.targetModRevision && context.items.length) {
    await selectRollbackVersion(context.items[0]);
  }
}

function renderRollbackVersions() {
  const context = state.rollbackContext;
  if (!context) return;
  const fragment = document.createDocumentFragment();
  for (const item of context.items) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "rollback-version-item";
    button.classList.toggle("is-selected", Number(item.entry.mod_revision) === Number(context.targetModRevision));
    button.setAttribute("aria-pressed", Number(item.entry.mod_revision) === Number(context.targetModRevision) ? "true" : "false");

    const title = document.createElement("span");
    title.textContent = `Key 版本 ${formatNumber(item.entry.version)}`;
    const source = document.createElement("small");
    source.textContent = rollbackSourceLabel(item.history_source);
    title.append(source);
    const revision = document.createElement("small");
    revision.textContent = `修改版本 #${formatNumber(item.entry.mod_revision)} · ${formatNumber(item.entry.value_bytes)} B`;
    const preview = document.createElement("em");
    preview.textContent = item.entry.value_preview || (item.entry.value_bytes ? "二进制 Value" : "(空值)");
    button.append(title, revision, preview);
    button.addEventListener("click", () => selectRollbackVersion(item));
    fragment.append(button);
  }
  el.rollbackVersionList.replaceChildren(fragment);
  el.rollbackLoadMoreButton.hidden = !context.nextBeforeModRevision;
  el.rollbackLoadMoreButton.disabled = false;
  el.rollbackLoadMoreButton.textContent = "加载更早版本";
}

async function selectRollbackVersion(item) {
  const context = state.rollbackContext;
  if (!context) return;
  context.targetModRevision = Number(item.entry.mod_revision);
  context.previewRequestID += 1;
  const requestID = context.previewRequestID;
  renderRollbackVersions();
  el.confirmRollbackButton.disabled = true;
  el.rollbackRevision.textContent = `#${formatNumber(item.entry.mod_revision)}`;
  el.rollbackSource.textContent = rollbackSourceLabel(item.history_source);
  el.rollbackEncoding.textContent = "读取中…";
  el.rollbackValuePreview.textContent = "正在读取该版本 Value…";
  const params = new URLSearchParams({
    cluster_id: context.clusterID,
    key_base64: context.keyBase64,
    expected_mod_revision: String(context.expectedModRevision),
    target_mod_revision: String(context.targetModRevision),
  });
  try {
    const preview = await api(`/api/v1/key/rollback-preview?${params}`);
    if (context !== state.rollbackContext || requestID !== context.previewRequestID) return;
    el.rollbackRevision.textContent = `#${formatNumber(preview.previous.mod_revision)}`;
    el.rollbackSource.textContent = rollbackSourceLabel(preview.history_source);
    el.rollbackEncoding.textContent = preview.previous.value_is_utf8 ? "UTF-8" : "Base64（二进制）";
    el.rollbackValuePreview.textContent = preview.previous.value_is_utf8
      ? preview.previous.value
      : preview.previous.value_base64;
    el.confirmRollbackButton.disabled = false;
  } catch (error) {
    if (context !== state.rollbackContext || requestID !== context.previewRequestID) return;
    el.rollbackValuePreview.textContent = error.message;
    toast(error.message, "error");
  }
}

function rollbackSourceLabel(source) {
  if (source === "etcd") return "etcd 历史";
  if (state.historyStorageType === "postgres") return "PostgreSQL 历史";
  if (state.historyStorageType === "mysql") return "MySQL 历史";
  return "本地文件历史";
}

async function rollbackSelected() {
  const context = state.rollbackContext;
  if (!context || !context.targetModRevision) return;
  el.confirmRollbackButton.disabled = true;
  try {
    await api("/api/v1/key/rollback", {
      method: "POST",
      body: JSON.stringify({
        cluster_id: context.clusterID,
        key_base64: context.keyBase64,
        expected_mod_revision: context.expectedModRevision,
        target_mod_revision: context.targetModRevision,
      }),
    });
    toast(`已回滚到修改版本 #${formatNumber(context.targetModRevision)}`);
    await loadKeys();
    await loadKey({ key_base64: context.keyBase64 });
  } catch (error) {
    toast(error.message, "error");
    if (error.code === "revision_conflict" && state.selected) loadKey(state.selected);
  } finally {
    state.rollbackContext = null;
    el.confirmRollbackButton.disabled = false;
  }
}

function changeEncoding() {
  const nextEncoding = el.encodingSelect.value;
  if (nextEncoding === state.valueEncoding) return;

  if (nextEncoding === "base64") {
    el.valueInput.value = bytesToBase64(new TextEncoder().encode(el.valueInput.value));
  } else {
    try {
      el.valueInput.value = new TextDecoder("utf-8", { fatal: true }).decode(base64ToBytes(el.valueInput.value.trim()));
    } catch {
      el.encodingSelect.value = "base64";
      toast("二进制数据无法安全转换为 UTF-8", "error");
      return;
    }
  }
  state.valueEncoding = nextEncoding;
  updateValueStats();
}

function updateValueStats() {
  const text = el.valueInput.value;
  if (state.valueEncoding === "base64") {
    try {
      const bytes = base64ToBytes(text.trim()).byteLength;
      el.valueStats.textContent = `${text.length.toLocaleString()} 字符 · 解码后 ${formatBytes(bytes)}`;
    } catch {
      el.valueStats.textContent = `${text.length.toLocaleString()} 字符 · Base64 格式无效`;
    }
  } else {
    const bytes = new TextEncoder().encode(text).byteLength;
    el.valueStats.textContent = `${[...text].length.toLocaleString()} 字符 · ${formatBytes(bytes)}`;
  }
}

function showListState(kind, message) {
  el.listState.hidden = false;
  el.listState.replaceChildren();
  if (kind === "loading") {
    const spinner = document.createElement("span");
    spinner.className = "spinner";
    spinner.setAttribute("aria-hidden", "true");
    el.listState.append(spinner);
  }
  const copy = document.createElement("p");
  if (kind !== "loading") {
    const title = document.createElement("strong");
    title.textContent = kind === "empty" ? "没有匹配结果" : "读取失败";
    copy.append(title);
  }
  copy.append(document.createTextNode(message));
  el.listState.append(copy);
}

function hideListState() {
  el.listState.hidden = true;
}

function updatePagination() {
  el.previousButton.disabled = state.cursorHistory.length === 0;
  el.nextButton.disabled = !state.nextCursor;
  el.pageLabel.textContent = `第 ${state.page} 页`;
}

function displayKey(item) {
  return item.key_is_utf8 === false ? `base64:${item.key_base64}` : item.key;
}

function formatBytes(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
}

function formatNumber(value) {
  return Number(value || 0).toLocaleString("zh-CN");
}

function bytesToBase64(bytes) {
  let binary = "";
  for (let index = 0; index < bytes.length; index += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(index, index + 0x8000));
  }
  return btoa(binary);
}

function base64ToBytes(value) {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
  return bytes;
}

async function copyText(text, label) {
  try {
    await navigator.clipboard.writeText(text);
    toast(`${label}已复制`);
  } catch {
    toast("复制失败，请手动复制", "error");
  }
}

function toast(message, type = "success") {
  const node = document.createElement("div");
  node.className = `toast ${type}`;
  node.textContent = message;
  el.toastRegion.append(node);
  window.setTimeout(() => node.remove(), 3200);
}

el.prefixForm.addEventListener("submit", (event) => {
  event.preventDefault();
  if (!state.selectedClusterId) {
    openClusterDialog();
    return;
  }
  state.prefix = el.prefixInput.value;
  loadKeys({ reset: true });
});
el.prefixInput.addEventListener("input", () => {
  el.clearPrefixButton.hidden = el.prefixInput.value.length === 0;
});
el.clearPrefixButton.addEventListener("click", () => {
  el.prefixInput.value = "";
  el.clearPrefixButton.hidden = true;
  el.prefixInput.focus();
});
el.connectionButton.addEventListener("click", checkConnection);
el.refreshButton.addEventListener("click", async () => {
  if (!state.selectedClusterId) return;
  await loadKeys();
  if (state.selected && !state.creating) await loadKey(state.selected);
});
el.newKeyButton.addEventListener("click", beginCreate);
el.emptyNewKeyButton.addEventListener("click", beginCreate);
el.cancelButton.addEventListener("click", closeEditor);
el.editorForm.addEventListener("submit", saveKey);
el.valueInput.addEventListener("input", updateValueStats);
el.encodingSelect.addEventListener("change", changeEncoding);
el.copyKeyInlineButton.addEventListener("click", () => copyText(el.keyInput.value, "Key "));
el.copyValueButton.addEventListener("click", () => copyText(el.valueInput.value, "Value "));
el.rollbackButton.addEventListener("click", previewRollback);
el.rollbackLoadMoreButton.addEventListener("click", async () => {
  try {
    await loadRollbackVersions({ append: true });
  } catch (error) {
    toast(error.message, "error");
    if (state.rollbackContext) renderRollbackVersions();
  }
});
el.rollbackDialog.addEventListener("close", () => {
  if (el.rollbackDialog.returnValue === "confirm") rollbackSelected();
  else state.rollbackContext = null;
});
el.deleteButton.addEventListener("click", () => {
  el.deleteKeyName.textContent = displayKey(state.selected);
  el.deleteDialog.showModal();
});
el.deleteDialog.addEventListener("close", () => {
  if (el.deleteDialog.returnValue === "confirm") deleteSelected();
});
el.clusterSelect.addEventListener("change", () => activateCluster(el.clusterSelect.value));
el.passwordChangeForm.addEventListener("submit", submitPasswordChange);
el.newPasswordInput.addEventListener("input", updatePasswordGuidance);
el.newPasswordConfirmInput.addEventListener("input", updatePasswordGuidance);
el.passwordChangeLogoutButton.addEventListener("click", logout);
el.loginForm.addEventListener("submit", submitLogin);
el.localLoginTab.addEventListener("click", () => selectLoginProvider("local"));
el.ldapLoginTab.addEventListener("click", () => selectLoginProvider("ldap"));
el.logoutButton.addEventListener("click", logout);
el.userPageButton.addEventListener("click", openUsersPage);
el.usersBackButton.addEventListener("click", closeUsersPage);
el.authSettingsForm.addEventListener("change", (event) => {
  if (event.target.name === "authMode") updateLDAPSettingsVisibility();
});
el.authSettingsForm.addEventListener("submit", saveAuthSettings);
el.ldapTLSModeSelect.addEventListener("change", () => {
  const port = Number(el.ldapPortInput.value || 0);
  if (!port || port === 389 || port === 636) {
    el.ldapPortInput.value = el.ldapTLSModeSelect.value === "ldaps" ? "636" : "389";
  }
});
el.testLDAPButton.addEventListener("click", testLDAPConnection);
el.addLocalUserButton.addEventListener("click", () => openUserEditor());
el.localRoleSelect.addEventListener("change", updateLocalUserClusterPermissionVisibility);
el.closeUserEditorButton.addEventListener("click", () => el.userEditorDialog.close());
el.cancelUserEditorButton.addEventListener("click", () => el.userEditorDialog.close());
el.userEditorForm.addEventListener("submit", saveLocalUser);
el.deleteLocalUserButton.addEventListener("click", requestDeleteLocalUser);
el.deleteLocalUserDialog.addEventListener("close", () => {
  if (el.deleteLocalUserDialog.returnValue === "confirm") deleteLocalUser();
});
el.auditLogButton.addEventListener("click", openAuditPage);
el.auditBackButton.addEventListener("click", closeAuditPage);
el.auditFilterForm.addEventListener("submit", (event) => {
  event.preventDefault();
  loadAudit({ reset: true });
});
el.auditRefreshButton.addEventListener("click", () => loadAudit());
el.auditPreviousButton.addEventListener("click", () => {
  if (!state.auditCursorHistory.length) return;
  state.auditCursor = state.auditCursorHistory.pop();
  state.auditPage -= 1;
  loadAudit();
});
el.auditNextButton.addEventListener("click", () => {
  if (!state.auditNextCursor) return;
  state.auditCursorHistory.push(state.auditCursor);
  state.auditCursor = state.auditNextCursor;
  state.auditPage += 1;
  loadAudit();
});
el.historySettingsButton.addEventListener("click", openHistorySettings);
el.historySettingsForm.addEventListener("submit", saveHistorySettings);
el.closeHistorySettingsButton.addEventListener("click", () => el.historySettingsDialog.close());
el.manageClustersButton.addEventListener("click", () => openClusterDialog(selectedCluster()));
el.closeClusterDialogButton.addEventListener("click", () => el.clusterDialog.close());
el.clusterForm.addEventListener("submit", saveCluster);
el.testClusterButton.addEventListener("click", testClusterConnection);
el.addAnotherClusterButton.addEventListener("click", () => openClusterDialog());
el.deleteClusterButton.addEventListener("click", () => {
  const cluster = state.clusters.find((item) => item.id === state.editingClusterId);
  el.deleteClusterName.textContent = cluster?.name || "该集群";
  el.deleteClusterDialog.showModal();
});
el.deleteClusterDialog.addEventListener("close", () => {
  if (el.deleteClusterDialog.returnValue === "confirm") deleteClusterConfiguration();
});
el.clearPasswordCheckbox.addEventListener("change", () => {
  el.clusterPasswordInput.disabled = el.clearPasswordCheckbox.checked;
  if (el.clearPasswordCheckbox.checked) el.clusterPasswordInput.value = "";
});
el.historyStorageForm.addEventListener("change", (event) => {
  if (event.target.name === "historyStorageType") {
    el.storageTestResult.hidden = true;
    updateHistoryStorageFields();
  }
});
el.historyStorageForm.addEventListener("submit", saveHistoryStorage);
el.testStorageButton.addEventListener("click", testHistoryStorageConnection);
el.historyStorageDialog.addEventListener("cancel", (event) => event.preventDefault());
el.previousButton.addEventListener("click", () => {
  if (!state.cursorHistory.length) return;
  state.cursor = state.cursorHistory.pop();
  state.page -= 1;
  loadKeys();
});
el.nextButton.addEventListener("click", () => {
  if (!state.nextCursor) return;
  state.cursorHistory.push(state.cursor);
  state.cursor = state.nextCursor;
  state.page += 1;
  loadKeys();
});
initializeAuthentication();
window.addEventListener("hashchange", showPageFromHash);
window.setInterval(() => {
  if (state.currentUser && !el.appMain.hidden) checkConnection();
}, 30000);
