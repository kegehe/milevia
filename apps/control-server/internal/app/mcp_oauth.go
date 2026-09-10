package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// MCP 远程 OAuth 2.1 + PKCE（P2）。
//
// 目标：需要授权的远程 MCP（http / sse）走标准 OAuth 2.1 授权码 + PKCE，把 access token
// 加密落库、按需刷新，并在注入（Claude / Codex）与连接测试时自动带上 Authorization。
//
// 发现链路依次尝试：
//  1. RFC 9728 保护资源元数据（.well-known/oauth-protected-resource[<path>]）取 authorization_servers；
//  2. RFC 8414 / OpenID Discovery 取 authorization_endpoint / token_endpoint / registration_endpoint；
//  3. RFC 7591 动态客户端注册（服务端支持时自动注册；不支持则要求用户预先配置 client_id）。
//
// 回调落在控制服务自身的 loopback 地址（ControlURL + /api/mcp/oauth/callback），因此不额外监听
// 端口。该路径不携带桌面会话，安全性由随机 state（32 字节）保证：state 只在发起方与服务端之间
// 流转，拿到它才能把授权码换成令牌，因此第三方无法凭猜测触发一次绑定。

const (
	// mcpOAuthCallbackPath 是 OAuth 回调路径（由浏览器直接跳转，必须放行会话校验）。
	mcpOAuthCallbackPath = "/api/mcp/oauth/callback"
	// mcpOAuthFlowTTL 是单个授权流程的最长存活时间。
	mcpOAuthFlowTTL = 10 * time.Minute
	// mcpOAuthFinishedTTL 是流程结束后保留记录的时间（供前端轮询到结果）。
	mcpOAuthFinishedTTL = 2 * time.Minute
	// mcpOAuthRefreshSkew 是提前刷新的余量：令牌距过期不足该值时先刷新再注入。
	mcpOAuthRefreshSkew = 60 * time.Second
	// mcpOAuthHTTPTimeout 是单次 OAuth HTTP 调用的超时。
	mcpOAuthHTTPTimeout = 20 * time.Second
	// mcpOAuthMaxBody 限制元数据 / 令牌响应体大小。
	mcpOAuthMaxBody = 1 << 20
)

// mcpOAuthTokenResponse 是令牌端点的响应。
type mcpOAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// mcpOAuthEndpoints 是一次发现得到的端点集合。
type mcpOAuthEndpoints struct {
	Resource              string
	AuthorizationEndpoint string
	TokenEndpoint         string
	RegistrationEndpoint  string
	ScopesSupported       []string
}

// mcpOAuthFlow 是一次进行中的授权流程（仅内存；令牌在回调时落库）。
type mcpOAuthFlow struct {
	ID               string
	ServerID         string
	ServerName       string
	State            string
	Verifier         string
	RedirectURI      string
	AuthorizationURL string
	Endpoints        mcpOAuthEndpoints
	ClientID         string
	ClientSecret     string
	Scope            string
	Status           string // pending | done | error
	Error            string
	ExpiresAt        time.Time
}

type mcpOAuthTokenRow struct {
	ServerID              string
	Resource              string
	AuthorizationEndpoint string
	TokenEndpoint         string
	RegistrationEndpoint  string
	ClientID              string
	ClientSecretRef       string
	AccessTokenRef        string
	RefreshTokenRef       string
	Scope                 string
	TokenType             string
	ExpiresAt             sql.NullTime
}

const mcpOAuthTokenColumns = `server_id,resource,authorization_endpoint,token_endpoint,registration_endpoint,client_id,client_secret_ref,access_token_ref,refresh_token_ref,scope,token_type,expires_at`

func migrateMCPOAuthTokens(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `create table if not exists mcp_oauth_tokens (
		server_id              text primary key,
		resource               text not null default '',
		authorization_endpoint text not null default '',
		token_endpoint         text not null default '',
		registration_endpoint  text not null default '',
		client_id              text not null default '',
		client_secret_ref      text not null default '',
		access_token_ref       text not null default '',
		refresh_token_ref      text not null default '',
		scope                  text not null default '',
		token_type             text not null default 'Bearer',
		expires_at             datetime,
		created_at             datetime not null,
		updated_at             datetime not null
	)`); err != nil {
		return fmt.Errorf("create mcp_oauth_tokens: %w", err)
	}
	return nil
}

func scanMCPOAuthToken(row interface{ Scan(dest ...any) error }) (mcpOAuthTokenRow, error) {
	var out mcpOAuthTokenRow
	err := row.Scan(
		&out.ServerID, &out.Resource, &out.AuthorizationEndpoint, &out.TokenEndpoint,
		&out.RegistrationEndpoint, &out.ClientID, &out.ClientSecretRef, &out.AccessTokenRef,
		&out.RefreshTokenRef, &out.Scope, &out.TokenType, &out.ExpiresAt,
	)
	return out, err
}

func (s *Server) loadMCPOAuthToken(ctx context.Context, serverID string) (mcpOAuthTokenRow, error) {
	return scanMCPOAuthToken(s.db.QueryRowContext(ctx,
		`select `+mcpOAuthTokenColumns+` from mcp_oauth_tokens where server_id=?`, serverID))
}

// ---------------------------------------------------------------------------
// 发现（RFC 9728 / RFC 8414）
// ---------------------------------------------------------------------------

func mcpOAuthHTTPClient() *http.Client {
	return &http.Client{Timeout: mcpOAuthHTTPTimeout}
}

// fetchJSONDocument 取一个 JSON 文档；任何失败（含非 2xx、解析失败）都返回 false，
// 由调用方决定是否继续尝试下一个候选地址——发现阶段需要容忍大量 404。
func fetchJSONDocument(ctx context.Context, client *http.Client, rawURL string, out any) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, mcpOAuthMaxBody))
	if err != nil {
		return false
	}
	return json.Unmarshal(body, out) == nil
}

func discoverMCPOAuthEndpoints(ctx context.Context, mcpURL string, client *http.Client) (mcpOAuthEndpoints, error) {
	parsed, err := url.Parse(strings.TrimSpace(mcpURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return mcpOAuthEndpoints{}, errors.New("MCP server 地址无效，无法发起 OAuth 授权")
	}
	if client == nil {
		client = mcpOAuthHTTPClient()
	}
	origin := parsed.Scheme + "://" + parsed.Host
	resourcePath := strings.TrimSuffix(parsed.Path, "/")

	// 1) 保护资源元数据：给出该资源由哪些授权服务器签发令牌。
	authServers := []string{}
	for _, candidate := range protectedResourceMetadataURLs(origin, resourcePath) {
		var doc struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
		}
		if !fetchJSONDocument(ctx, client, candidate, &doc) {
			continue
		}
		if len(doc.AuthorizationServers) > 0 {
			authServers = doc.AuthorizationServers
			break
		}
	}
	if len(authServers) == 0 {
		// 未提供保护资源元数据时，退化为「资源同源即授权服务器」这一常见部署形态。
		authServers = []string{origin}
	}

	// 2) 授权服务器元数据。
	for _, as := range authServers {
		for _, candidate := range authorizationServerMetadataURLs(as) {
			var doc struct {
				AuthorizationEndpoint string   `json:"authorization_endpoint"`
				TokenEndpoint         string   `json:"token_endpoint"`
				RegistrationEndpoint  string   `json:"registration_endpoint"`
				ScopesSupported       []string `json:"scopes_supported"`
			}
			if !fetchJSONDocument(ctx, client, candidate, &doc) {
				continue
			}
			if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
				continue
			}
			return mcpOAuthEndpoints{
				Resource:              mcpURL,
				AuthorizationEndpoint: doc.AuthorizationEndpoint,
				TokenEndpoint:         doc.TokenEndpoint,
				RegistrationEndpoint:  doc.RegistrationEndpoint,
				ScopesSupported:       doc.ScopesSupported,
			}, nil
		}
	}
	return mcpOAuthEndpoints{}, errors.New("未找到该 MCP server 的 OAuth 端点（.well-known 元数据缺失或不可达）")
}

// protectedResourceMetadataURLs 返回候选的保护资源元数据地址。RFC 9728 允许把资源路径
// 追加到 .well-known 段之后；两种形态都试，兼容不同实现。
func protectedResourceMetadataURLs(origin, resourcePath string) []string {
	out := []string{}
	if resourcePath != "" && resourcePath != "/" {
		out = append(out, origin+"/.well-known/oauth-protected-resource"+resourcePath)
	}
	return append(out, origin+"/.well-known/oauth-protected-resource")
}

// authorizationServerMetadataURLs 返回候选的授权服务器元数据地址（RFC 8414 + OpenID Discovery）。
func authorizationServerMetadataURLs(asURL string) []string {
	parsed, err := url.Parse(strings.TrimSpace(asURL))
	if err != nil || parsed.Host == "" {
		return nil
	}
	base := parsed.Scheme + "://" + parsed.Host
	path := strings.TrimSuffix(parsed.Path, "/")
	out := []string{}
	if path != "" {
		out = append(out, base+"/.well-known/oauth-authorization-server"+path)
		out = append(out, base+"/.well-known/openid-configuration"+path)
	}
	return append(out,
		base+"/.well-known/oauth-authorization-server",
		base+"/.well-known/openid-configuration",
	)
}

// registerMCPOAuthClient 走 RFC 7591 动态客户端注册。返回 client_id 与可选的 client_secret。
func registerMCPOAuthClient(ctx context.Context, client *http.Client, registrationEndpoint, redirectURI, clientName string) (string, string, error) {
	if client == nil {
		client = mcpOAuthHTTPClient()
	}
	payload, err := json.Marshal(map[string]any{
		"client_name":                clientName,
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"application_type":           "native",
	})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("动态客户端注册请求失败：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, mcpOAuthMaxBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("动态客户端注册失败（HTTP %d）：%s", resp.StatusCode, truncateProbeText(strings.TrimSpace(string(body))))
	}
	var doc struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", fmt.Errorf("解析注册响应失败：%w", err)
	}
	if doc.ClientID == "" {
		return "", "", errors.New("注册响应未返回 client_id")
	}
	return doc.ClientID, doc.ClientSecret, nil
}

// ---------------------------------------------------------------------------
// PKCE
// ---------------------------------------------------------------------------

func randomURLToken(byteLen int) (string, error) {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// mcpPKCEChallenge 由 verifier 派生 S256 challenge。
func mcpPKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// mcpOAuthRedirectURI 返回回调地址：控制服务自身的 loopback 地址。
func (s *Server) mcpOAuthRedirectURI() string {
	base := strings.TrimSuffix(strings.TrimSpace(s.config.ControlURL), "/")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	return base + mcpOAuthCallbackPath
}

// pruneMCPOAuthFlowsLocked 清理过期流程。调用方必须已持有 s.mu。
func (s *Server) pruneMCPOAuthFlowsLocked() {
	now := time.Now()
	for state, flow := range s.mcpOAuthFlows {
		if flow == nil || flow.ExpiresAt.Before(now) {
			delete(s.mcpOAuthFlows, state)
		}
	}
}

// ---------------------------------------------------------------------------
// 令牌交换与存储
// ---------------------------------------------------------------------------

func postMCPOAuthToken(ctx context.Context, client *http.Client, endpoint string, form url.Values, clientID, clientSecret string) (mcpOAuthTokenResponse, error) {
	if client == nil {
		client = mcpOAuthHTTPClient()
	}
	if strings.TrimSpace(endpoint) == "" {
		return mcpOAuthTokenResponse{}, errors.New("授权服务器未提供令牌端点")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return mcpOAuthTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if clientSecret != "" {
		req.SetBasicAuth(clientID, clientSecret)
	}
	resp, err := client.Do(req)
	if err != nil {
		return mcpOAuthTokenResponse{}, fmt.Errorf("令牌端点请求失败：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, mcpOAuthMaxBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mcpOAuthTokenResponse{}, fmt.Errorf("令牌端点返回 HTTP %d：%s", resp.StatusCode, truncateProbeText(strings.TrimSpace(string(body))))
	}
	var token mcpOAuthTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return mcpOAuthTokenResponse{}, fmt.Errorf("解析令牌响应失败：%w", err)
	}
	if token.AccessToken == "" {
		return mcpOAuthTokenResponse{}, errors.New("令牌端点未返回 access_token")
	}
	return token, nil
}

func exchangeMCPOAuthCode(ctx context.Context, client *http.Client, flow *mcpOAuthFlow, code string) (mcpOAuthTokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", flow.RedirectURI)
	form.Set("client_id", flow.ClientID)
	form.Set("code_verifier", flow.Verifier)
	if flow.Endpoints.Resource != "" {
		form.Set("resource", flow.Endpoints.Resource)
	}
	return postMCPOAuthToken(ctx, client, flow.Endpoints.TokenEndpoint, form, flow.ClientID, flow.ClientSecret)
}

// storeMCPOAuthToken 加密保存令牌（access / refresh / client_secret 一律走 sec_ 引用），
// 并吊销被替换掉的旧引用。
func (s *Server) storeMCPOAuthToken(ctx context.Context, flow *mcpOAuthFlow, token mcpOAuthTokenResponse) error {
	// 先读旧行：SQLite 单连接，事务持有连接时不能再发查询。
	old, oldErr := s.loadMCPOAuthToken(ctx, flow.ServerID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	created := []string{}
	rollback := func() {
		for _, id := range created {
			_ = s.profileSecrets.Revoke(tx, ctx, id)
		}
	}
	accessRef, err := s.profileSecrets.Store(tx, ctx, token.AccessToken)
	if err != nil {
		return err
	}
	created = append(created, accessRef)
	refreshRef := ""
	if token.RefreshToken != "" {
		refreshRef, err = s.profileSecrets.Store(tx, ctx, token.RefreshToken)
		if err != nil {
			rollback()
			return err
		}
		created = append(created, refreshRef)
	} else if oldErr == nil {
		// 部分授权服务器刷新时不返回新的 refresh token，沿用旧值。
		refreshRef = old.RefreshTokenRef
	}
	secretRef := ""
	if flow.ClientSecret != "" {
		secretRef, err = s.profileSecrets.Store(tx, ctx, flow.ClientSecret)
		if err != nil {
			rollback()
			return err
		}
		created = append(created, secretRef)
	} else if oldErr == nil {
		secretRef = old.ClientSecretRef
	}
	tokenType := token.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	scope := token.Scope
	if scope == "" {
		scope = flow.Scope
	}
	expiresAt := sql.NullTime{}
	if token.ExpiresIn > 0 {
		expiresAt = sql.NullTime{Time: time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second), Valid: true}
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `insert into mcp_oauth_tokens (`+mcpOAuthTokenColumns+`,created_at,updated_at)
		values (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		on conflict(server_id) do update set
			resource=excluded.resource,authorization_endpoint=excluded.authorization_endpoint,
			token_endpoint=excluded.token_endpoint,registration_endpoint=excluded.registration_endpoint,
			client_id=excluded.client_id,client_secret_ref=excluded.client_secret_ref,
			access_token_ref=excluded.access_token_ref,refresh_token_ref=excluded.refresh_token_ref,
			scope=excluded.scope,token_type=excluded.token_type,expires_at=excluded.expires_at,
			updated_at=excluded.updated_at`,
		flow.ServerID, flow.Endpoints.Resource, flow.Endpoints.AuthorizationEndpoint, flow.Endpoints.TokenEndpoint,
		flow.Endpoints.RegistrationEndpoint, flow.ClientID, secretRef, accessRef, refreshRef, scope, tokenType,
		expiresAt, now, now); err != nil {
		rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if oldErr == nil {
		for _, ref := range []string{old.AccessTokenRef, old.RefreshTokenRef, old.ClientSecretRef} {
			if ref == "" || ref == accessRef || ref == refreshRef || ref == secretRef {
				continue
			}
			_ = s.profileSecrets.Revoke(s.db, ctx, ref)
		}
	}
	return nil
}

// mcpOAuthAuthorizationHeader 返回该 server 当前可用的 Authorization 头值（形如 "Bearer xxx"）。
// 令牌临近过期且存在 refresh token 时先刷新；刷新失败则退回现有令牌（可能已过期），
// 让对端给出明确错误，而不是静默不注入。
func (s *Server) mcpOAuthAuthorizationHeader(ctx context.Context, serverID string) (string, bool) {
	row, err := s.loadMCPOAuthToken(ctx, serverID)
	if err != nil || row.AccessTokenRef == "" {
		return "", false
	}
	if row.ExpiresAt.Valid && time.Now().Add(mcpOAuthRefreshSkew).After(row.ExpiresAt.Time) {
		if token, ok := s.refreshMCPOAuthToken(ctx, row); ok {
			return "Bearer " + token, true
		}
	}
	plain, err := s.profileSecrets.Load(s.db, ctx, row.AccessTokenRef)
	if err != nil || plain == "" {
		return "", false
	}
	return "Bearer " + plain, true
}

// refreshMCPOAuthToken 用 refresh token 换新的 access token 并落库。
// 刷新发生在运行路径上，因此使用独立的超时上下文，避免受调用方 ctx 生命周期影响。
func (s *Server) refreshMCPOAuthToken(ctx context.Context, row mcpOAuthTokenRow) (string, bool) {
	if row.RefreshTokenRef == "" {
		return "", false
	}
	refreshToken, err := s.profileSecrets.Load(s.db, ctx, row.RefreshTokenRef)
	if err != nil || refreshToken == "" {
		return "", false
	}
	clientSecret := ""
	if row.ClientSecretRef != "" {
		if plain, err := s.profileSecrets.Load(s.db, ctx, row.ClientSecretRef); err == nil {
			clientSecret = plain
		}
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", row.ClientID)
	if row.Resource != "" {
		form.Set("resource", row.Resource)
	}
	refreshCtx, cancel := context.WithTimeout(context.Background(), mcpOAuthHTTPTimeout)
	defer cancel()
	token, err := postMCPOAuthToken(refreshCtx, mcpOAuthHTTPClient(), row.TokenEndpoint, form, row.ClientID, clientSecret)
	if err != nil {
		return "", false
	}
	flow := &mcpOAuthFlow{
		ServerID: row.ServerID,
		Endpoints: mcpOAuthEndpoints{
			Resource:              row.Resource,
			AuthorizationEndpoint: row.AuthorizationEndpoint,
			TokenEndpoint:         row.TokenEndpoint,
			RegistrationEndpoint:  row.RegistrationEndpoint,
		},
		ClientID:     row.ClientID,
		ClientSecret: clientSecret,
		Scope:        row.Scope,
	}
	if err := s.storeMCPOAuthToken(refreshCtx, flow, token); err != nil {
		return "", false
	}
	return token.AccessToken, true
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

type mcpOAuthStartResponse struct {
	FlowID           string `json:"flowId"`
	AuthorizationURL string `json:"authorizationUrl"`
	RedirectURI      string `json:"redirectUri"`
	Scope            string `json:"scope,omitempty"`
	ClientID         string `json:"clientId,omitempty"`
}

func (s *Server) startMCPServerOAuth(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverID")
	var input struct {
		Scope string `json:"scope"`
	}
	if !decodeOptional(w, r, &input) {
		return
	}
	ctx := r.Context()
	stored, err := s.fetchStoredMCPServer(ctx, serverID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("MCP server not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if stored.Transport != mcpTransportHTTP && stored.Transport != mcpTransportSSE {
		writeError(w, http.StatusBadRequest, errors.New("只有 http / sse 类型的 MCP server 需要 OAuth 授权"))
		return
	}
	projectPath := ""
	if stored.ProjectID != "" {
		if project, err := s.getProjectByID(ctx, stored.ProjectID); err == nil {
			projectPath = project.Path
		}
	}
	target := s.resolveAgentTargetEnv("", projectPath)
	mcpURL := resolveMCPPlaceholders(stored.URL, target, projectPath)
	if strings.TrimSpace(mcpURL) == "" {
		writeError(w, http.StatusBadRequest, errors.New("MCP server 未配置地址"))
		return
	}

	client := mcpOAuthHTTPClient()
	endpoints, err := discoverMCPOAuthEndpoints(ctx, mcpURL, client)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	// 复用已注册的客户端，避免每次授权都在服务商侧留下一个新应用。
	existing, existingErr := s.loadMCPOAuthToken(ctx, serverID)
	clientID, clientSecret := "", ""
	if existingErr == nil && existing.ClientID != "" && existing.RegistrationEndpoint == endpoints.RegistrationEndpoint {
		clientID = existing.ClientID
		if existing.ClientSecretRef != "" {
			if plain, err := s.profileSecrets.Load(s.db, ctx, existing.ClientSecretRef); err == nil {
				clientSecret = plain
			}
		}
	}
	if clientID == "" {
		if endpoints.RegistrationEndpoint == "" {
			writeError(w, http.StatusBadGateway, errors.New("该授权服务器不支持动态客户端注册，需要先在服务商处注册客户端并配置 client_id"))
			return
		}
		clientID, clientSecret, err = registerMCPOAuthClient(ctx, client, endpoints.RegistrationEndpoint, s.mcpOAuthRedirectURI(), "Milevia")
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
	}

	state, err := randomURLToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("无法生成授权状态"))
		return
	}
	verifier, err := randomURLToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("无法生成 PKCE 校验值"))
		return
	}
	redirectURI := s.mcpOAuthRedirectURI()
	// 默认不请求任何 scope：多数服务商的 scopes_supported 只是「支持清单」，全量请求会
	// 造成过度授权。需要特定 scope 时由调用方显式给出。
	scope := strings.TrimSpace(input.Scope)

	authURL, err := url.Parse(endpoints.AuthorizationEndpoint)
	if err != nil {
		writeError(w, http.StatusBadGateway, errors.New("授权端点地址无效"))
		return
	}
	query := authURL.Query()
	query.Set("response_type", "code")
	query.Set("client_id", clientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("state", state)
	query.Set("code_challenge", mcpPKCEChallenge(verifier))
	query.Set("code_challenge_method", "S256")
	query.Set("resource", mcpURL)
	if scope != "" {
		query.Set("scope", scope)
	}
	authURL.RawQuery = query.Encode()

	flow := &mcpOAuthFlow{
		ID:               uuid.NewString(),
		ServerID:         stored.ID,
		ServerName:       stored.Name,
		State:            state,
		Verifier:         verifier,
		RedirectURI:      redirectURI,
		AuthorizationURL: authURL.String(),
		Endpoints:        endpoints,
		ClientID:         clientID,
		ClientSecret:     clientSecret,
		Scope:            scope,
		Status:           "pending",
		ExpiresAt:        time.Now().Add(mcpOAuthFlowTTL),
	}
	s.mu.Lock()
	s.pruneMCPOAuthFlowsLocked()
	s.mcpOAuthFlows[state] = flow
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, mcpOAuthStartResponse{
		FlowID:           flow.ID,
		AuthorizationURL: flow.AuthorizationURL,
		RedirectURI:      redirectURI,
		Scope:            scope,
		ClientID:         clientID,
	})
}

// handleMCPOAuthCallback 接收浏览器回调，换取并保存令牌。该路径不要求桌面会话，
// 安全性依托随机 state（见文件头说明）。
func (s *Server) handleMCPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	state := query.Get("state")
	code := query.Get("code")
	oauthErr := query.Get("error")
	description := query.Get("error_description")

	s.mu.Lock()
	flow, ok := s.mcpOAuthFlows[state]
	if ok {
		delete(s.mcpOAuthFlows, state)
	}
	s.mu.Unlock()
	if !ok || flow == nil {
		writeMCPOAuthResultPage(w, http.StatusBadRequest, false, "授权失败：state 无效或已过期，请回到 Milevia 重新发起授权。")
		return
	}

	// 流程结束后保留短暂记录，供前端轮询到最终结果。
	finish := func(status, message string) {
		flow.Status = status
		flow.Error = message
		flow.ExpiresAt = time.Now().Add(mcpOAuthFinishedTTL)
		s.mu.Lock()
		s.mcpOAuthFlows[state] = flow
		s.mu.Unlock()
	}
	fail := func(message string) {
		finish("error", message)
		writeMCPOAuthResultPage(w, http.StatusBadRequest, false, message)
	}

	if oauthErr != "" {
		if description != "" {
			fail("授权被拒绝：" + description)
		} else {
			fail("授权被拒绝：" + oauthErr)
		}
		return
	}
	if strings.TrimSpace(code) == "" {
		fail("授权回调缺少 code 参数，无法换取令牌。")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mcpOAuthHTTPTimeout)
	defer cancel()
	token, err := exchangeMCPOAuthCode(ctx, mcpOAuthHTTPClient(), flow, code)
	if err != nil {
		fail("换取访问令牌失败：" + err.Error())
		return
	}
	if err := s.storeMCPOAuthToken(ctx, flow, token); err != nil {
		fail("保存访问令牌失败：" + err.Error())
		return
	}
	finish("done", "")
	writeMCPOAuthResultPage(w, http.StatusOK, true, "授权完成，现在可以关闭此页面并回到 Milevia。")
}

type mcpOAuthStatus struct {
	ServerID              string `json:"serverId"`
	Authorized            bool   `json:"authorized"`
	Scope                 string `json:"scope,omitempty"`
	TokenType             string `json:"tokenType,omitempty"`
	ExpiresAt             string `json:"expiresAt,omitempty"`
	Expired               bool   `json:"expired"`
	HasRefreshToken       bool   `json:"hasRefreshToken"`
	HasClientSecret       bool   `json:"hasClientSecret"`
	ClientID              string `json:"clientId,omitempty"`
	AuthorizationEndpoint string `json:"authorizationEndpoint,omitempty"`
	TokenEndpoint         string `json:"tokenEndpoint,omitempty"`
}

func (s *Server) getMCPServerOAuth(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverID")
	row, err := s.loadMCPOAuthToken(r.Context(), serverID)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, mcpOAuthStatus{ServerID: serverID})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	status := mcpOAuthStatus{
		ServerID:              serverID,
		Authorized:            row.AccessTokenRef != "",
		Scope:                 row.Scope,
		TokenType:             row.TokenType,
		HasRefreshToken:       row.RefreshTokenRef != "",
		HasClientSecret:       row.ClientSecretRef != "",
		ClientID:              row.ClientID,
		AuthorizationEndpoint: row.AuthorizationEndpoint,
		TokenEndpoint:         row.TokenEndpoint,
	}
	if row.ExpiresAt.Valid {
		status.ExpiresAt = row.ExpiresAt.Time.UTC().Format(time.RFC3339)
		status.Expired = time.Now().After(row.ExpiresAt.Time)
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) deleteMCPServerOAuth(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverID")
	ctx := r.Context()
	row, err := s.loadMCPOAuthToken(ctx, serverID)
	if errors.Is(err, sql.ErrNoRows) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := s.db.ExecContext(ctx, `delete from mcp_oauth_tokens where server_id=?`, serverID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, ref := range []string{row.AccessTokenRef, row.RefreshTokenRef, row.ClientSecretRef} {
		_ = s.profileSecrets.Revoke(s.db, ctx, ref)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getMCPOAuthFlow(w http.ResponseWriter, r *http.Request) {
	flowID := chi.URLParam(r, "flowID")
	s.mu.Lock()
	var found *mcpOAuthFlow
	for _, flow := range s.mcpOAuthFlows {
		if flow != nil && flow.ID == flowID {
			found = flow
			break
		}
	}
	s.mu.Unlock()
	if found == nil {
		writeError(w, http.StatusNotFound, errors.New("授权流程不存在或已过期"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"flowId":   found.ID,
		"serverId": found.ServerID,
		"status":   found.Status,
		"error":    found.Error,
	})
}

// writeMCPOAuthResultPage 返回一个极简结果页。不含任何令牌内容。
func writeMCPOAuthResultPage(w http.ResponseWriter, status int, ok bool, message string) {
	title := "授权失败"
	accent := "#c0392b"
	if ok {
		title = "授权完成"
		accent = "#07c160"
	}
	body := "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">" +
		"<title>" + title + "</title></head>" +
		"<body style=\"margin:0;font-family:system-ui,-apple-system,'Segoe UI',sans-serif;background:#f8f5ef;color:#20232e;display:flex;min-height:100vh;align-items:center;justify-content:center\">" +
		"<main style=\"max-width:420px;padding:32px;text-align:center\">" +
		"<h1 style=\"margin:0 0 12px;font-size:20px;color:" + accent + "\">" + title + "</h1>" +
		"<p style=\"margin:0;line-height:1.7;font-size:14px\">" + htmlEscape(message) + "</p>" +
		"</main></body></html>"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func htmlEscape(text string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return replacer.Replace(text)
}

// hasMCPHeaderKey 报告请求头集合里是否存在某个键（大小写不敏感）。
func hasMCPHeaderKey(values map[string]string, want string) bool {
	for key := range values {
		if strings.EqualFold(strings.TrimSpace(key), want) {
			return true
		}
	}
	return false
}

// mcpOAuthHeaderValue 返回 OAuth 的 Authorization 头值。
//
// useEnvRefs 为真（Windows/WSL）时返回 ${MCP_SEC_*} 占位符与对应的环境变量增项，
// 令牌不落盘；为假（SSH 远端，无安全 env 通道）时内联。
func (s *Server) mcpOAuthHeaderValue(ctx context.Context, serverID string, useEnvRefs bool) (string, string, bool) {
	header, ok := s.mcpOAuthAuthorizationHeader(ctx, serverID)
	if !ok {
		return "", "", false
	}
	if !useEnvRefs {
		return header, "", true
	}
	name := mcpSecretEnvName(mcpSecretRefPrefix + sanitizeMCPRunKey(serverID))
	return "${" + name + "}", name + "=" + header, true
}
