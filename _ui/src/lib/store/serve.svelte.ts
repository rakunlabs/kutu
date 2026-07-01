// File-serving store — manages the single ServeSettings document that
// drives kutu's built-in FTP / SFTP / TFTP / WebDAV servers, plus the
// live per-protocol runtime status. All persistence goes through
// /api/v1/serve*.
//
// Saving reconciles the running servers server-side, so after an update
// we re-load both the settings (to pick up a generated SFTP host key)
// and the status (to reflect the new bind state).

import axios from 'axios';
import { addToast } from './toast.svelte';
import type { ServeSettings, ServeStatus } from '@/lib/types/config';

function emptySettings(): ServeSettings {
  return {
    ftp: { enabled: false },
    sftp: { enabled: false },
    tftp: { enabled: false },
    webdav: { enabled: false },
    users: [],
    shares: [],
  };
}

let settings = $state<ServeSettings>(emptySettings());
let status = $state<ServeStatus[]>([]);
let loaded = $state(false);
let loading = $state(false);
let saving = $state(false);

async function load(): Promise<void> {
  loading = true;
  const [cfgRes, stRes] = await Promise.allSettled([
    axios.get<ServeSettings>('/api/v1/serve'),
    axios.get<ServeStatus[]>('/api/v1/serve/status'),
  ]);
  if (cfgRes.status === 'fulfilled') {
    settings = normalize(cfgRes.value.data);
  }
  if (stRes.status === 'fulfilled') status = stRes.value.data ?? [];
  loaded = true;
  loading = false;
}

async function refreshStatus(): Promise<void> {
  try {
    const res = await axios.get<ServeStatus[]>('/api/v1/serve/status');
    status = res.data ?? [];
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

// normalize fills in the optional list fields so the UI can bind to
// them without null guards.
function normalize(s: ServeSettings | null | undefined): ServeSettings {
  const base = emptySettings();
  if (!s) return base;
  return {
    ftp: { ...base.ftp, ...s.ftp },
    sftp: { ...base.sftp, ...s.sftp },
    tftp: { ...base.tftp, ...s.tftp },
    webdav: { ...base.webdav, ...s.webdav },
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
};
