import axios from 'axios';

// AppInfo mirrors GET /api/v1/info. kutu runs without authentication, so
// the auth/user/vault fields from pika are gone; what remains is build
// metadata, the capability vocabulary (always fully granted) and the
// at-rest key status used to drive the unlock screen.
export interface AppInfo {
  name: string;
  version: string;
  commit?: string;
  date?: string;
  capabilities?: string[];
  auth_enabled?: boolean;
  key_initialized?: boolean;
  key_unlocked?: boolean;
  // True when the process was started with an encryption.password that
  // didn't match the on-disk verifier (server stays locked).
  encryption_config_invalid?: boolean;
}

// createAppStore is a trimmed app store: kutu has no login, so identity
// is always null, every capability check passes, and logout is a no-op.
function createAppStore() {
  let info = $state<AppInfo | null>(null);
  // Always "authenticated" — there is no login flow in kutu.
  const identity = null;

  async function loadInfo(): Promise<void> {
    try {
      const response = await axios.get('/api/v1/info');
      info = response.data;
    } catch {
      info = { name: 'kutu', version: 'unknown', capabilities: [] };
    }
  }

  // kutu has no authorization; every capability is granted.
  function hasPermission(_key: string): boolean {
    return true;
  }

  function hasAnyPermission(..._keys: string[]): boolean {
    return true;
  }

  // No-op: there is no session to end.
  async function logout(): Promise<void> {}

  return {
    get info() {
      return info;
    },
    get identity() {
      return identity;
    },
    get authenticated() {
      return true;
    },
    loadInfo,
    hasPermission,
    hasAnyPermission,
    logout,
  };
}

export const appStore = createAppStore();
