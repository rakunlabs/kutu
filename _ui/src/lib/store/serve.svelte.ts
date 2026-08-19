// File-serving store — manages the single ServeSettings document that
// drives kutu's built-in file servers (any number of FTP / SFTP / TFTP /
// WebDAV / S3 instances) plus the live per-instance runtime status. All
// persistence goes through /api/v1/serve*.
//
// The document is one JSONB blob server-side, but the UI edits it in
// slices (servers / shares / users), so besides the whole-document
// save() there are partial helpers that replace one slice of the last
// persisted document and PUT the result — that way saving a user never
// sends along a half-edited server form.
//
// Saving reconciles the running servers server-side, so after an update
// we re-load both the settings (to pick up a generated SFTP host key)
// and the status (to reflect the new bind state).

import axios from 'axios';
import { addToast } from './toast.svelte';
import type { ServeSettings, ServeStatus, ServeServerEntry, ServeShare, ServeUser } from '@/lib/types/config';

function emptySettings(): ServeSettings {
  return { servers: [], users: [], shares: [] };
}

let settings = $state<ServeSettings>(emptySettings());
let status = $state<ServeStatus[]>([]);
let loaded = $state(false);
let loading = $state(false);
let saving = $state(false);

function normalizeStatus(value: unknown): ServeStatus[] {
  return Array.isArray(value) ? value : [];
}

async function load(): Promise<void> {
  loading = true;
  const [cfgRes, stRes] = await Promise.allSettled([
    axios.get<ServeSettings>('/api/v1/serve'),
    axios.get<ServeStatus[]>('/api/v1/serve/status'),
  ]);
  if (cfgRes.status === 'fulfilled') {
    settings = normalize(cfgRes.value.data);
  }
  if (stRes.status === 'fulfilled') status = normalizeStatus(stRes.value.data);
  loaded = true;
  loading = false;
}

async function refreshStatus(): Promise<void> {
  try {
    const res = await axios.get<ServeStatus[]>('/api/v1/serve/status');
    status = normalizeStatus(res.data);
  } catch {/* status is best-effort */}
}

async function save(next: ServeSettings): Promise<boolean> {
  saving = true;
  try {
    const res = await axios.put<ServeSettings>('/api/v1/serve', next);
    settings = normalize(res.data);
    addToast('Serve settings saved.', 'success');
    await refreshStatus();
    return true;
  } catch (error: any) {
    const msg = error.response?.data?.message
      || error.response?.data
      || error.message
      || 'Failed to save serve settings';
    addToast(typeof msg === 'string' ? msg : 'Failed to save serve settings', 'alert');
    return false;
  } finally {
    saving = false;
  }
}

// snapshot returns a plain (non-reactive) deep copy of the persisted
// document, ready to be sliced and PUT back.
function snapshot(): ServeSettings {
  return structuredClone($state.snapshot(settings)) as ServeSettings;
}

// ── partial saves: replace one slice of the persisted document ──

async function saveServers(servers: ServeServerEntry[]): Promise<boolean> {
  return save({ ...snapshot(), servers });
}

async function saveShares(shares: ServeShare[]): Promise<boolean> {
  return save({ ...snapshot(), shares });
}

async function saveUsers(users: ServeUser[]): Promise<boolean> {
  return save({ ...snapshot(), users });
}

// normalize fills in the optional list fields so the UI can bind to
// them without null guards.
function normalize(s: ServeSettings | null | undefined): ServeSettings {
  if (!s) return emptySettings();
  return {
    servers: (s.servers ?? []).map(sv => ({ ...sv, shares: sv.shares ?? [] })),
    users: s.users ?? [],
    shares: s.shares ?? [],
  };
}

export const serveStore = {
  get settings() { return settings; },
  get status() { return status; },
  get loaded() { return loaded; },
  get loading() { return loading; },
  get saving() { return saving; },
  load,
  refreshStatus,
  save,
  saveServers,
  saveShares,
  saveUsers,
};
