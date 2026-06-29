import axios from 'axios';

import { addToast } from '@/lib/store/toast.svelte';

// Trimmed config store. kutu only keeps the registry tree here (the
// Registries page reads/saves the whole namespace+repo tree). Reads go
// through GET /api/v1/registries; saves are translated into the granular
// namespace/repo endpoints by diffing against the loaded tree.

export interface RegistryRepository {
  name: string;
  type: string;
  kind: string;
  description?: string;
  [k: string]: unknown;
}

export interface RegistryNamespace {
  name: string;
  description?: string;
  repositories?: RegistryRepository[];
}

export interface RegistrySettings {
  disabled?: boolean;
  namespaces?: RegistryNamespace[];
}

export interface Settings {
  registry?: RegistrySettings;
}

const enc = (s: string) => encodeURIComponent(s);

function createConfigStore() {
  let settings = $state<Settings | null>(null);

  async function loadSettings(): Promise<void> {
    try {
      const res = await axios.get<RegistrySettings>('/api/v1/registries');
      settings = { registry: res.data ?? { namespaces: [] } };
    } catch (e: any) {
      addToast(e?.response?.data?.message ?? 'Failed to load registries', 'alert');
    }
  }

  // saveRegistrySettings persists the whole desired tree by diffing it
  // against the currently-loaded one and issuing granular create/update/
  // delete calls (the backend has no whole-tree replace endpoint).
  async function saveRegistrySettings(next: RegistrySettings): Promise<void> {
    const prev = settings?.registry ?? { namespaces: [] };
    try {
      await axios.put('/api/v1/registries', { disabled: !!next.disabled });
      await applyRegistryDiff(prev, next);
      await loadSettings();
      addToast('Registry settings saved', 'success');
    } catch (e: any) {
      addToast(e?.response?.data?.message ?? 'Failed to save registry settings', 'alert');
      throw e;
    }
  }

  return {
    get settings() {
      return settings;
    },
    loadSettings,
    saveRegistrySettings,
  };
}

async function applyRegistryDiff(prev: RegistrySettings, next: RegistrySettings): Promise<void> {
  const prevNS = new Map((prev.namespaces ?? []).map((n) => [n.name, n]));
  const nextNS = new Map((next.namespaces ?? []).map((n) => [n.name, n]));

  // Deleted namespaces (cascades repositories server-side).
  for (const name of prevNS.keys()) {
    if (!nextNS.has(name)) {
      await axios.delete(`/api/v1/registries/namespaces/${enc(name)}`);
    }
  }

  for (const [name, ns] of nextNS) {
    const before = prevNS.get(name);
    if (!before) {
      await axios.post('/api/v1/registries/namespaces', {
        name: ns.name,
        description: ns.description ?? '',
      });
    } else if ((before.description ?? '') !== (ns.description ?? '')) {
      await axios.put(`/api/v1/registries/namespaces/${enc(name)}`, {
        description: ns.description ?? '',
      });
    }

    const prevRepos = new Map((before?.repositories ?? []).map((r) => [r.name, r]));
    const nextRepos = new Map((ns.repositories ?? []).map((r) => [r.name, r]));

    for (const rn of prevRepos.keys()) {
      if (!nextRepos.has(rn)) {
        await axios.delete(`/api/v1/registries/namespaces/${enc(name)}/repos/${enc(rn)}`);
      }
    }
    for (const [rn, repo] of nextRepos) {
      if (!prevRepos.has(rn)) {
        await axios.post(`/api/v1/registries/namespaces/${enc(name)}/repos`, repo);
      } else {
        await axios.put(`/api/v1/registries/namespaces/${enc(name)}/repos/${enc(rn)}`, repo);
      }
    }
  }
}

export const configStore = createConfigStore();
