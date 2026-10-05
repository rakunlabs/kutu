import axios from 'axios';

// Access tokens (GET/POST /api/v1/tokens, PATCH/DELETE /api/v1/tokens/{id}).
// Requires tokens.manage.
//
// Scope paths: "raw/<mount>/<glob>", "registry/<ns>/<repo>/**", or "**"
// for everything. `*` matches one segment, `**` any number.

export type TokenOperation = 'read' | 'write' | 'delete' | '*';

export interface TokenScope {
  path: string;
  operations: TokenOperation[];
}

export interface TokenInfo {
  id: string;
  name: string;
  scopes: TokenScope[];
  created_at: string;
  created_by?: string;
  expires_at?: string;
  active: boolean;
  /** Updated in batches server-side, so it can lag by about a minute. */
  last_used_at?: string;
}

export interface CreateTokenRequest {
  name: string;
  scopes: TokenScope[];
  expires_at?: string;
}

/** Includes the raw key, which is only ever returned once. */
export interface CreateTokenResponse extends TokenInfo {
  raw_key: string;
}

export interface PatchTokenRequest {
  name?: string;
  scopes?: TokenScope[];
  active?: boolean;
  expires_at?: string;
}

function createTokenStore() {
  let tokens = $state<TokenInfo[]>([]);
  let loaded = $state(false);

  async function load(): Promise<void> {
    try {
      const response = await axios.get<TokenInfo[]>('/api/v1/tokens');
      tokens = Array.isArray(response.data) ? response.data : [];
    } catch {
      tokens = [];
    } finally {
      loaded = true;
    }
  }

  async function create(req: CreateTokenRequest): Promise<CreateTokenResponse> {
    const response = await axios.post<CreateTokenResponse>('/api/v1/tokens', req);
    await load();
    return response.data;
  }

  async function remove(id: string): Promise<void> {
    await axios.delete(`/api/v1/tokens/${id}`);
    await load();
  }

  async function patch(id: string, req: PatchTokenRequest): Promise<void> {
    await axios.patch(`/api/v1/tokens/${id}`, req);
    await load();
  }

  return {
    get tokens() { return tokens; },
    get loaded() { return loaded; },
    load,
    create,
    remove,
    patch,
  };
}

export const tokenStore = createTokenStore();
