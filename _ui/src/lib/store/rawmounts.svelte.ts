// Raw mounts store — manages the configured raw-mount backends that
// the file browser browses, the registries store artifacts on, and the
// proxy raw handler serves. All persistence goes through
// /api/v1/raw-mounts*.
//
// Two slices are kept in sync because the API exposes two views:
//   - `mounts`  : runtime summary (prefix/type/writable) — only the
//                 mounts that successfully materialized. Feeds the file
//                 tree and the registry mount pickers.
//   - `configs` : full persisted config (incl. credentials) — every
//                 row, even one whose backend is momentarily down.
//                 Feeds the management editor.
//
// Mutations re-load both slices so a freshly created mount shows up in
// the picker the instant its backend builds, without a manual refresh.

import axios from 'axios';
import { addToast } from './toast.svelte';
import type { RawMount, RawMountConfig } from '@/lib/types/config';

let mounts = $state<RawMount[]>([]);
let configs = $state<RawMountConfig[]>([]);
let loaded = $state(false);
let loading = $state(false);

async function load(): Promise<void> {
  loading = true;
  // allSettled so one failing endpoint doesn't blank the other slice.
  const [sumRes, cfgRes] = await Promise.allSettled([
    axios.get<RawMount[]>('/api/v1/raw-mounts'),
    axios.get<RawMountConfig[]>('/api/v1/raw-mounts/configs'),
  ]);
  if (sumRes.status === 'fulfilled') mounts = sumRes.value.data ?? [];
  if (cfgRes.status === 'fulfilled') configs = cfgRes.value.data ?? [];
  loaded = true;
  loading = false;
}

async function create(m: RawMountConfig): Promise<RawMountConfig> {
  try {
    const res = await axios.post<RawMountConfig>('/api/v1/raw-mounts', m);
    addToast('Raw mount created.', 'success');
    await load();
    return res.data;
  } catch (error: any) {
    surface(error, 'create');
    throw error;
  }
}

async function update(m: RawMountConfig): Promise<RawMountConfig> {
  try {
    const res = await axios.put<RawMountConfig>(
      `/api/v1/raw-mounts/${encodeURIComponent(m.prefix)}`,
      m,
    );
    addToast('Raw mount saved.', 'success');
    await load();
    return res.data;
  } catch (error: any) {
    surface(error, 'save');
    throw error;
  }
}

async function remove(prefix: string): Promise<void> {
  try {
    await axios.delete(`/api/v1/raw-mounts/${encodeURIComponent(prefix)}`);
    addToast('Raw mount deleted.', 'success');
    await load();
  } catch (error: any) {
    surface(error, 'delete');
    throw error;
  }
}

function surface(error: any, action: string) {
  const msg = error.response?.data?.message
    || error.response?.data
    || error.message
    || `Failed to ${action} raw mount`;
  addToast(typeof msg === 'string' ? msg : `Failed to ${action} raw mount`, 'alert');
}

export const rawMountsStore = {
  get mounts() { return mounts; },
  get configs() { return configs; },
  get loaded() { return loaded; },
  get loading() { return loading; },
  load,
  create,
  update,
  remove,
};
