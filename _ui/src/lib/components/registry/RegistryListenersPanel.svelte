<script lang="ts">
  import { onMount } from 'svelte';
  import {
    ArrowLeft,
    Copy,
    Loader2,
    LockKeyhole,
    Pencil,
    Plus,
    RadioTower,
    Trash2,
  } from 'lucide-svelte';
  import { addToast } from '@/lib/store/toast.svelte';
  import * as registryAPI from '@/lib/store/registry.svelte';
  import type { Namespace } from '@/lib/components/registry/types';
  import type { RegistryListener, RegistryListenerStatus } from '@/lib/store/registry.svelte';

  let {
    namespaces,
    canAdmin,
    onClose,
  }: {
    namespaces: Namespace[];
    canAdmin: boolean;
    onClose: () => void;
  } = $props();

  let listeners = $state<RegistryListener[]>([]);
  let status = $state<RegistryListenerStatus[]>([]);
  let loading = $state(true);
  let saving = $state(false);
  let editingId = $state<string | null>(null);
  let draft = $state<RegistryListener>(emptyDraft());

  const inputCls = 'w-full rounded border border-warm-300 dark:border-warm-700 bg-white dark:bg-warm-900 px-2.5 py-1.5 text-xs outline-none focus:border-accent-500 focus:ring-1 focus:ring-accent-500/20';
  const labelCls = 'flex flex-col gap-1 text-xs font-medium text-warm-600 dark:text-warm-300';

  function emptyDraft(): RegistryListener {
    const namespace = namespaces[0]?.name ?? '';
    const repo = namespaces[0]?.repositories?.[0]?.name ?? '';
    return {
      id: '',
      name: '',
      enabled: true,
      host: '',
      port: 5000,
      hostname: '',
      namespace,
      repo,
      tls_cert_pem: '',
      tls_key_pem: '',
    };
  }

  const draftRepos = $derived(
    namespaces.find((ns) => ns.name === draft.namespace)?.repositories ?? [],
  );

  function statusFor(id: string): RegistryListenerStatus | undefined {
    return status.find((row) => row.id === id);
  }

  async function load() {
    loading = true;
    try {
      const [cfg, live] = await Promise.all([
        registryAPI.listListeners(),
        registryAPI.listListenerStatus(),
      ]);
      listeners = cfg.listeners ?? [];
      status = Array.isArray(live) ? live : [];
    } catch (err) {
      addToast(`Failed to load registry listeners: ${err}`, 'alert');
    } finally {
      loading = false;
    }
  }

  function openNew() {
    draft = emptyDraft();
    editingId = 'new';
  }

  function openEdit(listener: RegistryListener) {
    draft = structuredClone($state.snapshot(listener));
    editingId = listener.id;
  }

  function closeEditor() {
    editingId = null;
  }

  function changeNamespace() {
    const repos = namespaces.find((ns) => ns.name === draft.namespace)?.repositories ?? [];
    if (!repos.some((repo) => repo.name === draft.repo)) {
      draft.repo = repos[0]?.name ?? '';
    }
  }

  function normalizedDraft(): RegistryListener {
    return {
      id: draft.id,
      name: draft.name?.trim() || undefined,
      enabled: draft.enabled,
      host: draft.host?.trim() || undefined,
      port: Number(draft.port),
      hostname: draft.hostname?.trim().toLowerCase() || undefined,
      namespace: draft.namespace,
      repo: draft.repo,
      tls_cert_pem: draft.tls_cert_pem?.trim() || undefined,
      tls_key_pem: draft.tls_key_pem?.trim() || undefined,
    };
  }

  async function persist(next: RegistryListener[], successMessage: string): Promise<boolean> {
    saving = true;
    try {
      const saved = await registryAPI.saveListeners(next);
      listeners = saved.listeners ?? [];
      status = await registryAPI.listListenerStatus();
      addToast(successMessage, 'success');
      return true;
    } catch (err) {
      addToast(`Could not save listeners: ${err}`, 'alert');
      return false;
    } finally {
      saving = false;
    }
  }

  async function saveDraft() {
    const row = normalizedDraft();
    if (!row.namespace || !row.repo) {
      addToast('Choose a namespace and repository.', 'alert');
      return;
    }
    if (!Number.isInteger(row.port) || row.port < 1 || row.port > 65535) {
      addToast('Port must be between 1 and 65535.', 'alert');
      return;
    }
    if (!!row.tls_cert_pem !== !!row.tls_key_pem) {
      addToast('TLS requires both the certificate and private key.', 'alert');
      return;
    }

    const next = editingId === 'new'
      ? [...listeners.map((item) => structuredClone($state.snapshot(item))), row]
      : listeners.map((item) => item.id === editingId ? row : structuredClone($state.snapshot(item)));
    if (await persist(next, 'Registry listener saved.')) closeEditor();
  }

  async function toggleEnabled(listener: RegistryListener) {
    const next = listeners.map((item) => {
      const copy = structuredClone($state.snapshot(item));
      if (copy.id === listener.id) copy.enabled = !copy.enabled;
      return copy;
    });
    await persist(next, listener.enabled ? 'Registry listener stopped.' : 'Registry listener started.');
  }

  async function deleteListener(listener: RegistryListener) {
    const label = listener.name || `${listener.namespace}/${listener.repo}`;
    if (!confirm(`Delete listener "${label}"? Active connections to this endpoint will be dropped.`)) return;
    const next = listeners
      .filter((item) => item.id !== listener.id)
      .map((item) => structuredClone($state.snapshot(item)));
    if (await persist(next, 'Registry listener deleted.') && editingId === listener.id) closeEditor();
  }

  function endpoint(listener: RegistryListener): string {
    const st = statusFor(listener.id);
    const scheme = st?.tls || listener.tls_cert_pem ? 'https' : 'http';
    const host = listener.hostname || listener.host || 'localhost';
    return `${scheme}://${host}:${listener.port}`;
  }

  async function copyEndpoint(listener: RegistryListener) {
    await navigator.clipboard.writeText(endpoint(listener));
    addToast('Endpoint copied.', 'success');
  }

  onMount(load);
</script>

<div class="flex-1 overflow-y-auto bg-warm-50 dark:bg-warm-950">
  <div class="mx-auto max-w-5xl px-4 py-5 sm:px-6">
    <div class="flex flex-wrap items-start justify-between gap-3 border-b border-warm-200 dark:border-warm-800 pb-4">
      <div class="flex items-start gap-3">
        <button
          type="button"
          class="mt-0.5 rounded p-1.5 text-warm-500 hover:bg-warm-200 dark:hover:bg-warm-800 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-500"
          onclick={onClose}
          title="Back to repositories"
          aria-label="Back to repositories"
        >
          <ArrowLeft size={16} />
        </button>
        <div>
          <h2 class="flex items-center gap-2 text-base font-semibold text-warm-900 dark:text-warm-100">
            <RadioTower size={17} class="text-accent-500" /> Registry listeners
          </h2>
          <p class="mt-1 max-w-2xl text-xs leading-relaxed text-warm-500 dark:text-warm-400">
            Publish a repository at the root of a dedicated endpoint. Different hostnames can share one port;
            TLS is optional when a reverse proxy terminates HTTPS in front of Kutu.
          </p>
        </div>
      </div>
      {#if canAdmin}
        <button
          type="button"
          class="inline-flex items-center gap-1.5 rounded bg-accent-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-accent-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-500 focus-visible:ring-offset-2 disabled:opacity-50"
          onclick={openNew}
          disabled={saving || namespaces.length === 0}
        >
          <Plus size={13} /> New listener
        </button>
      {/if}
    </div>

    <div class="mt-4 rounded border border-warm-200 dark:border-warm-800 bg-white dark:bg-warm-900 px-3 py-2.5 text-xs text-warm-600 dark:text-warm-300">
      <span class="font-medium">Shared ports:</span>
      use a unique hostname for each endpoint on the same port. A blank hostname is the catch-all and can appear only once.
      HTTP-only endpoints still route by the <code class="font-mono text-[11px]">Host</code> header behind a reverse proxy.
    </div>

    {#if loading}
      <div class="flex items-center justify-center py-16 text-sm text-warm-500">
        <Loader2 size={18} class="mr-2 animate-spin" /> Loading listeners…
      </div>
    {:else if listeners.length === 0 && editingId !== 'new'}
      <div class="mt-5 flex flex-col items-center justify-center rounded border border-dashed border-warm-300 dark:border-warm-700 px-6 py-14 text-center">
        <RadioTower size={28} class="mb-3 text-warm-400" />
        <h3 class="text-sm font-semibold text-warm-700 dark:text-warm-200">No dedicated endpoints yet</h3>
        <p class="mt-1 max-w-lg text-xs leading-relaxed text-warm-500 dark:text-warm-400">
          The main <code class="font-mono">/registries/…</code> routes remain available. Add a listener when a client needs a root-level URL,
          a private port, or its own hostname and certificate.
        </p>
        {#if canAdmin}
          <button
            type="button"
            class="mt-4 inline-flex items-center gap-1.5 rounded bg-accent-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-accent-700"
            onclick={openNew}
          >
            <Plus size={13} /> Create first listener
          </button>
        {/if}
      </div>
    {:else}
      <div class="mt-5 overflow-hidden rounded border border-warm-200 dark:border-warm-800 bg-white dark:bg-warm-900">
        {#each listeners as listener (listener.id)}
          {@const live = statusFor(listener.id)}
          <div class="grid gap-3 border-b border-warm-100 dark:border-warm-800 px-4 py-3 last:border-b-0 md:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)_auto] md:items-center">
            <div class="min-w-0">
              <div class="flex items-center gap-2">
                <span class={`h-2 w-2 shrink-0 rounded-full ${live?.running ? 'bg-emerald-500' : live?.error ? 'bg-vermilion-500' : 'bg-warm-300 dark:bg-warm-600'}`}></span>
                <button
                  type="button"
                  class="truncate text-left text-sm font-semibold text-warm-800 hover:text-accent-600 dark:text-warm-100 dark:hover:text-accent-400"
                  onclick={() => openEdit(listener)}
                  disabled={!canAdmin}
                >
                  {listener.name || `${listener.namespace}/${listener.repo}`}
                </button>
                {#if live?.shared}
                  <span class="rounded bg-accent-50 dark:bg-accent-950/30 px-1.5 py-0.5 text-[10px] text-accent-700 dark:text-accent-300">shared port</span>
                {/if}
                {#if live?.tls}
                  <span class="inline-flex items-center gap-1 rounded bg-warm-100 dark:bg-warm-800 px-1.5 py-0.5 text-[10px] text-warm-600 dark:text-warm-300"><LockKeyhole size={9} /> TLS</span>
                {/if}
              </div>
              <div class="mt-1 truncate pl-4 text-[11px] text-warm-500 dark:text-warm-400">
                <span class="font-mono">{listener.namespace}/{listener.repo}</span>
                <span class="mx-1.5">·</span>
                {live?.running ? 'running' : live?.error ? 'bind error' : 'stopped'}
              </div>
              {#if live?.error}
                <p class="mt-1 pl-4 text-[11px] text-vermilion-700 dark:text-vermilion-300 break-all">{live.error}</p>
              {/if}
            </div>

            <button
              type="button"
              class="group flex min-w-0 items-center gap-2 text-left"
              onclick={() => copyEndpoint(listener)}
              title="Copy endpoint"
            >
              <code class="truncate text-[11px] text-warm-600 group-hover:text-accent-600 dark:text-warm-300 dark:group-hover:text-accent-400">{endpoint(listener)}</code>
              <Copy size={11} class="shrink-0 text-warm-400" />
            </button>

            {#if canAdmin}
              <div class="flex items-center justify-end gap-1.5">
                <label class="inline-flex cursor-pointer items-center" title={listener.enabled ? 'Stop listener' : 'Start listener'}>
                  <input
                    type="checkbox"
                    class="peer sr-only"
                    checked={listener.enabled}
                    disabled={saving}
                    onchange={() => toggleEnabled(listener)}
                  />
                  <span class="relative h-5 w-9 rounded-full bg-warm-200 transition-colors after:absolute after:left-0.5 after:top-0.5 after:h-4 after:w-4 after:rounded-full after:bg-white after:transition-transform peer-checked:bg-accent-600 peer-checked:after:translate-x-4 dark:bg-warm-700"></span>
                </label>
                <button type="button" class="rounded p-1.5 text-warm-500 hover:bg-warm-100 hover:text-accent-600 dark:hover:bg-warm-800" onclick={() => openEdit(listener)} title="Edit listener" aria-label="Edit listener"><Pencil size={13} /></button>
                <button type="button" class="rounded p-1.5 text-warm-500 hover:bg-vermilion-50 hover:text-vermilion-700 dark:hover:bg-vermilion-950/30 dark:hover:text-vermilion-300" onclick={() => deleteListener(listener)} title="Delete listener" aria-label="Delete listener"><Trash2 size={13} /></button>
              </div>
            {/if}
          </div>
        {/each}
      </div>
    {/if}

    {#if editingId !== null}
      <section class="mt-5 rounded border border-warm-200 dark:border-warm-800 bg-white dark:bg-warm-900 p-4 sm:p-5">
        <div class="flex items-center justify-between gap-3">
          <h3 class="text-sm font-semibold text-warm-800 dark:text-warm-100">
            {editingId === 'new' ? 'New registry listener' : 'Edit registry listener'}
          </h3>
          <span class="text-[11px] text-warm-400">Changes take effect immediately after save</span>
        </div>

        <div class="mt-4 grid grid-cols-1 gap-3 sm:grid-cols-2">
          <label class={labelCls}>Name (optional)<input class={inputCls} bind:value={draft.name} placeholder="Docker production" /></label>
          <label class={labelCls}>
            Namespace
            <select class={inputCls} bind:value={draft.namespace} onchange={changeNamespace}>
              {#each namespaces as ns (ns.name)}<option value={ns.name}>{ns.name}</option>{/each}
            </select>
          </label>
          <label class={labelCls}>
            Repository
            <select class={inputCls} bind:value={draft.repo}>
              {#each draftRepos as repo (repo.name)}<option value={repo.name}>{repo.name} · {repo.type} / {repo.kind}</option>{/each}
            </select>
          </label>
          <label class={labelCls}>Bind host<input class={inputCls + ' font-mono'} bind:value={draft.host} placeholder="0.0.0.0" /></label>
          <label class={labelCls}>
            Hostname (optional)
            <input class={inputCls + ' font-mono'} bind:value={draft.hostname} placeholder="docker.example.com" />
            <span class="font-normal text-[10px] leading-relaxed text-warm-400">Blank makes this the catch-all on its port. Supports <code class="font-mono">*.example.com</code>.</span>
          </label>
          <label class={labelCls}>Port<input type="number" min="1" max="65535" class={inputCls + ' font-mono tabular-nums'} bind:value={draft.port} /></label>
        </div>

        <details class="mt-4 rounded border border-warm-200 dark:border-warm-700 bg-warm-50/70 dark:bg-warm-950/30" open={!!draft.tls_cert_pem || !!draft.tls_key_pem}>
          <summary class="cursor-pointer select-none px-3 py-2 text-xs font-medium text-warm-700 dark:text-warm-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent-500">
            TLS certificate <span class="font-normal text-warm-400">(optional)</span>
          </summary>
          <div class="grid grid-cols-1 gap-3 border-t border-warm-200 dark:border-warm-700 p-3">
            <p class="text-[11px] leading-relaxed text-warm-500 dark:text-warm-400">
              Leave both fields blank for plain HTTP, including deployments behind a TLS-terminating reverse proxy.
              Every endpoint sharing a TLS port must provide its own matching certificate.
            </p>
            <label class={labelCls}>Certificate chain (PEM)<textarea class={inputCls + ' min-h-24 font-mono'} bind:value={draft.tls_cert_pem} placeholder="-----BEGIN CERTIFICATE-----"></textarea></label>
            <label class={labelCls}>Private key (PEM)<textarea class={inputCls + ' min-h-24 font-mono'} bind:value={draft.tls_key_pem} placeholder="-----BEGIN PRIVATE KEY-----"></textarea></label>
          </div>
        </details>

        <div class="mt-4 flex flex-wrap items-center justify-between gap-3">
          <label class="inline-flex cursor-pointer items-center gap-2 text-xs text-warm-600 dark:text-warm-300">
            <input type="checkbox" bind:checked={draft.enabled} /> Enabled after save
          </label>
          <div class="flex items-center gap-2">
            <button type="button" class="rounded border border-warm-300 dark:border-warm-700 px-3 py-1.5 text-xs hover:bg-warm-100 dark:hover:bg-warm-800" onclick={closeEditor} disabled={saving}>Cancel</button>
            <button type="button" class="inline-flex items-center gap-1.5 rounded bg-accent-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-accent-700 disabled:cursor-not-allowed disabled:opacity-50" onclick={saveDraft} disabled={saving || draftRepos.length === 0}>
              {#if saving}<Loader2 size={12} class="animate-spin" />{/if}
              Save listener
            </button>
          </div>
        </div>
      </section>
    {/if}
  </div>
</div>
