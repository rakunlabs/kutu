<script lang="ts">
 // Raw mounts panel — operator-facing CRUD for the storage backends
 // the file browser, registries and proxy raw handler all read from.
 // A raw mount maps a single prefix (the first path segment of
 // /raw/<prefix>/...) to one of six backends: a local directory, S3,
 // FTP, SFTP, WebDAV or Vercel Blob.
 //
 // Modelled on ProxyListenersPanel: a card grid of existing mounts
 // plus a single inline editor drawer whose fields switch on the
 // selected backend type. List rows come from the persisted `configs`
 // slice (so a mount shows even when its backend is momentarily
 // unreachable); the `mounts` slice supplies the live writable/active
 // badge.
 //
 // Surface tokens (DESIGN_SYSTEM.md): page = dark:bg-warm-900,
 // card = dark:bg-warm-800, elevated input = dark:bg-warm-900.
 import type { RawMount, RawMountConfig, RawMountType } from '@/lib/types/config';
 import { rawMountsStore } from '@/lib/store/rawmounts.svelte';
 import { HardDrive, Plus, Trash2, Cloud, Server, Folder, Globe, Database } from 'lucide-svelte';

 let {
  configs,
  mounts,
 }: {
  configs: RawMountConfig[];
  mounts: RawMount[];
 } = $props();

 // editingId tracks which mount prefix is open in the drawer. `null`
 // collapses it; "new" opens a fresh draft.
 let editingId = $state<string | null>(null);
 let draft = $state<RawMountConfig>(emptyDraft());
 let saving = $state(false);

 const TYPES: { value: RawMountType; label: string }[] = [
  { value: 'local', label: 'Local directory' },
  { value: 's3', label: 'S3 / compatible' },
  { value: 'ftp', label: 'FTP / FTPS' },
  { value: 'sftp', label: 'SFTP (SSH)' },
  { value: 'webdav', label: 'WebDAV' },
  { value: 'vercel-blob', label: 'Vercel Blob' },
 ];

 function emptyDraft(): RawMountConfig {
  return { prefix: '', type: 'local', path: '' };
 }

 // ensureSub guarantees the sub-config object for the current type
 // exists so the template can bind into it without a null guard.
 function ensureSub() {
  switch (draft.type) {
   case 's3': draft.s3 ??= { bucket: '' }; break;
   case 'ftp': draft.ftp ??= { host: '' }; break;
   case 'sftp': draft.sftp ??= { host: '' }; break;
   case 'webdav': draft.webdav ??= { url: '' }; break;
   case 'vercel-blob': draft.vercelBlob ??= { token: '' }; break;
   default: break;
  }
 }

 function summaryFor(prefix: string): RawMount | undefined {
  return mounts.find(m => m.prefix === prefix);
 }

 function typeIcon(type?: string) {
  switch (type) {
   case 's3': return Cloud;
   case 'ftp': return Server;
   case 'sftp': return Server;
   case 'webdav': return Globe;
   case 'vercel-blob': return Database;
   default: return Folder;
  }
 }

 function openNew() {
  draft = emptyDraft();
  editingId = 'new';
 }

 function openEdit(cfg: RawMountConfig) {
  // Deep clone so editing the drawer never mutates the store row
  // until the user saves.
  draft = structuredClone($state.snapshot(cfg)) as RawMountConfig;
  draft.type ??= 'local';
  ensureSub();
  editingId = cfg.prefix;
 }

 function closeEditor() {
  editingId = null;
 }

 // cleanDraft strips sub-configs that don't belong to the selected
 // backend so we never persist stale credentials from a type the
 // operator clicked through and away from.
 function cleanDraft(): RawMountConfig {
  const t = draft.type ?? 'local';
  const out: RawMountConfig = { prefix: draft.prefix.trim(), type: t };
  if (t === 'local') out.path = draft.path?.trim() ?? '';
  else if (t === 's3') out.s3 = draft.s3;
  else if (t === 'ftp') out.ftp = draft.ftp;
  else if (t === 'sftp') out.sftp = draft.sftp;
  else if (t === 'webdav') out.webdav = draft.webdav;
  else if (t === 'vercel-blob') out.vercelBlob = draft.vercelBlob;
  return out;
 }

 async function saveDraft() {
  ensureSub();
  saving = true;
  try {
   const payload = cleanDraft();
   if (editingId === 'new') {
    await rawMountsStore.create(payload);
   } else {
    await rawMountsStore.update(payload);
   }
   closeEditor();
  } catch {/* toast in store */}
  finally { saving = false; }
 }

 async function deleteMount(cfg: RawMountConfig) {
  if (!confirm(`Delete raw mount "${cfg.prefix}"? Repositories or proxies pointing at it will stop resolving.`)) return;
  try { await rawMountsStore.remove(cfg.prefix); } catch {/* toast in store */}
 }

 // input/label class fragments kept terse so the markup stays readable.
 const inputCls = 'rounded border border-slate-200 dark:border-warm-700 bg-white dark:bg-warm-900 px-2 py-1 text-xs';
 const labelCls = 'text-xs text-slate-600 dark:text-slate-300 flex flex-col gap-1';
</script>

<div>
 <header class="mb-5 flex items-start justify-between gap-4">
  <div>
   <h2 class="text-base font-semibold text-slate-800 dark:text-slate-100 flex items-center gap-2">
    <HardDrive size={18} class="text-accent-600 dark:text-accent-400" />
    Raw mounts
   </h2>
   <p class="text-sm text-slate-500 dark:text-slate-400 mt-1">
    Storage backends exposed under <code class="font-mono">/raw/&lt;prefix&gt;</code>. Browse them on the
    Files page and point local / remote registries at them.
   </p>
  </div>
  <button
   type="button"
   class="px-3 py-1.5 text-xs rounded bg-accent-600 text-white font-medium hover:bg-accent-700 inline-flex items-center gap-1.5 cursor-pointer shrink-0"
   onclick={openNew}
  >
   <Plus size={12} /> New raw mount
  </button>
 </header>

 {#if configs.length === 0 && editingId !== 'new'}
  <div class="flex flex-col items-center justify-center py-16 px-6 text-center text-slate-400 dark:text-slate-500 border border-dashed border-slate-200 dark:border-warm-700 rounded-lg">
   <HardDrive size={28} class="mb-3 opacity-40" />
   <div class="text-sm font-medium text-slate-600 dark:text-slate-300 mb-1">
    No raw mounts configured yet
   </div>
   <div class="text-xs mb-4 max-w-md">
    Add a mount to expose a directory or remote storage. Registries and the file browser
    can only use prefixes that exist here.
   </div>
   <button
    type="button"
    class="px-3 py-1.5 text-xs rounded bg-accent-600 text-white font-medium hover:bg-accent-700 inline-flex items-center gap-1.5 cursor-pointer"
    onclick={openNew}
   >
    <Plus size={12} /> New raw mount
   </button>
  </div>
 {:else}
  <div class="grid gap-3 md:grid-cols-2 lg:grid-cols-3">
   {#each configs as cfg (cfg.prefix)}
    {@const sum = summaryFor(cfg.prefix)}
    {@const Icon = typeIcon(cfg.type)}
    <div class="bg-white dark:bg-warm-800 border border-slate-200 dark:border-warm-700 rounded-lg p-4 flex flex-col">
     <div class="flex items-start gap-2 mb-2">
      <Icon size={16} class="text-accent-600 dark:text-accent-400 shrink-0 mt-0.5" />
      <div class="grow min-w-0">
       <button
        type="button"
        class="text-sm font-semibold text-slate-800 dark:text-slate-100 hover:text-accent-600 dark:hover:text-accent-400 truncate text-left cursor-pointer font-mono"
        onclick={() => openEdit(cfg)}
       >
        {cfg.prefix}
       </button>
       <div class="text-xs text-slate-500 dark:text-slate-400 font-mono truncate">
        {cfg.type ?? 'local'}{#if cfg.type === 'local' || !cfg.type}{cfg.path ? ` - ${cfg.path}` : ''}{/if}
       </div>
      </div>
      <span class={'shrink-0 text-[10px] uppercase tracking-wide px-1.5 py-0.5 rounded ' + (sum
       ? (sum.writable
         ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-300'
         : 'bg-slate-100 text-slate-600 dark:bg-warm-900 dark:text-slate-400')
       : 'bg-amber-100 text-amber-700 dark:bg-amber-950/40 dark:text-amber-300')}>
       {sum ? (sum.writable ? 'writable' : 'read-only') : 'inactive'}
      </span>
     </div>

     {#if !sum}
      <div class="mb-3 p-2 rounded text-xs bg-amber-50 dark:bg-amber-950/30 border border-amber-200 dark:border-amber-800 text-amber-800 dark:text-amber-200">
       Backend not active — check the configuration or that the target is reachable.
      </div>
     {/if}

     <div class="mt-auto flex items-center justify-between gap-2 pt-2">
      <button
       type="button"
       class="text-xs text-accent-600 dark:text-accent-400 hover:text-accent-700 dark:hover:text-accent-300 cursor-pointer"
       onclick={() => openEdit(cfg)}
      >Edit</button>
      <button
       type="button"
       class="px-2 py-1 rounded text-xs text-vermilion-700 dark:text-vermilion-300 hover:bg-vermilion-50 dark:hover:bg-vermilion-950/40 inline-flex items-center gap-1 cursor-pointer"
       title="Delete raw mount"
       onclick={() => deleteMount(cfg)}
      >
       <Trash2 size={12} />
      </button>
     </div>
    </div>
   {/each}
  </div>
 {/if}

 {#if editingId !== null}
  <div class="mt-6 bg-white dark:bg-warm-800 border border-slate-200 dark:border-warm-700 rounded-lg p-5">
   <h3 class="text-sm font-semibold text-slate-800 dark:text-slate-100 mb-3">
    {editingId === 'new' ? 'New raw mount' : `Edit "${editingId}"`}
   </h3>

   <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
    <label class={labelCls}>
     Prefix
     <input
      type="text"
      class={inputCls + ' font-mono'}
      bind:value={draft.prefix}
      disabled={editingId !== 'new'}
      placeholder="data"
     />
     <span class="text-[10px] text-slate-400">Single path segment. Served at /raw/&lt;prefix&gt;.</span>
    </label>

    <label class={labelCls}>
     Backend type
     <select class={inputCls} bind:value={draft.type} onchange={ensureSub}>
      {#each TYPES as t (t.value)}
       <option value={t.value}>{t.label}</option>
      {/each}
     </select>
    </label>
   </div>

   <!-- ── Per-backend fields ── -->
   <div class="mt-3 rounded border border-slate-200 dark:border-warm-700 p-3 bg-slate-50/60 dark:bg-warm-900/40">
    {#if draft.type === 'local' || !draft.type}
     <label class={labelCls}>
      Directory path
      <input type="text" class={inputCls + ' font-mono'} bind:value={draft.path} placeholder="/var/lib/kutu/data" />
      <span class="text-[10px] text-slate-400">Must exist on the server and be a directory.</span>
     </label>
    {:else if draft.type === 's3'}
     {#if draft.s3}
      <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
       <label class={labelCls}>Bucket<input type="text" class={inputCls + ' font-mono'} bind:value={draft.s3.bucket} placeholder="my-bucket" /></label>
       <label class={labelCls}>Region<input type="text" class={inputCls + ' font-mono'} bind:value={draft.s3.region} placeholder="us-east-1" /></label>
       <label class={labelCls + ' md:col-span-2'}>Endpoint (S3-compatible; empty = AWS)<input type="text" class={inputCls + ' font-mono'} bind:value={draft.s3.endpoint} placeholder="https://minio.example.com" /></label>
       <label class={labelCls}>Access key<input type="text" class={inputCls + ' font-mono'} bind:value={draft.s3.access_key} autocomplete="off" /></label>
       <label class={labelCls}>Secret key<input type="password" class={inputCls + ' font-mono'} bind:value={draft.s3.secret_key} autocomplete="off" /></label>
       <label class={labelCls}>Key prefix (optional)<input type="text" class={inputCls + ' font-mono'} bind:value={draft.s3.prefix} placeholder="artifacts/" /></label>
       <div class="flex items-end gap-4">
        <label class="text-xs text-slate-600 dark:text-slate-300 inline-flex items-center gap-2"><input type="checkbox" bind:checked={draft.s3.path_style} /> Path-style</label>
        <label class="text-xs text-slate-600 dark:text-slate-300 inline-flex items-center gap-2"><input type="checkbox" bind:checked={draft.s3.secure} /> TLS (https)</label>
       </div>
      </div>
     {/if}
    {:else if draft.type === 'ftp'}
     {#if draft.ftp}
      <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
       <label class={labelCls}>Host<input type="text" class={inputCls + ' font-mono'} bind:value={draft.ftp.host} placeholder="ftp.example.com:21" /></label>
       <label class={labelCls}>Base path (optional)<input type="text" class={inputCls + ' font-mono'} bind:value={draft.ftp.base_path} placeholder="/pub" /></label>
       <label class={labelCls}>Username<input type="text" class={inputCls + ' font-mono'} bind:value={draft.ftp.username} autocomplete="off" /></label>
       <label class={labelCls}>Password<input type="password" class={inputCls + ' font-mono'} bind:value={draft.ftp.password} autocomplete="off" /></label>
       <label class="text-xs text-slate-600 dark:text-slate-300 inline-flex items-center gap-2 md:col-span-2"><input type="checkbox" bind:checked={draft.ftp.tls} /> Use explicit TLS (FTPS)</label>
      </div>
     {/if}
    {:else if draft.type === 'sftp'}
     {#if draft.sftp}
      <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
       <label class={labelCls}>Host<input type="text" class={inputCls + ' font-mono'} bind:value={draft.sftp.host} placeholder="sftp.example.com:22" /></label>
       <label class={labelCls}>Base path (optional)<input type="text" class={inputCls + ' font-mono'} bind:value={draft.sftp.base_path} placeholder="/upload" /></label>
       <label class={labelCls}>Username<input type="text" class={inputCls + ' font-mono'} bind:value={draft.sftp.username} autocomplete="off" /></label>
       <label class={labelCls}>Password<input type="password" class={inputCls + ' font-mono'} bind:value={draft.sftp.password} autocomplete="off" /></label>
       <label class={labelCls + ' md:col-span-2'}>Private key (PEM, optional — overrides password)<textarea class={inputCls + ' font-mono'} rows="3" bind:value={draft.sftp.private_key} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea></label>
      </div>
     {/if}
    {:else if draft.type === 'webdav'}
     {#if draft.webdav}
      <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
       <label class={labelCls + ' md:col-span-2'}>URL<input type="text" class={inputCls + ' font-mono'} bind:value={draft.webdav.url} placeholder="https://dav.example.com/remote.php/dav" /></label>
       <label class={labelCls}>Username<input type="text" class={inputCls + ' font-mono'} bind:value={draft.webdav.username} autocomplete="off" /></label>
       <label class={labelCls}>Password<input type="password" class={inputCls + ' font-mono'} bind:value={draft.webdav.password} autocomplete="off" /></label>
       <label class={labelCls + ' md:col-span-2'}>Base path (optional)<input type="text" class={inputCls + ' font-mono'} bind:value={draft.webdav.base_path} placeholder="/files" /></label>
      </div>
     {/if}
    {:else if draft.type === 'vercel-blob'}
     {#if draft.vercelBlob}
      <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
       <label class={labelCls + ' md:col-span-2'}>Token<input type="password" class={inputCls + ' font-mono'} bind:value={draft.vercelBlob.token} placeholder="vercel_blob_rw_..." autocomplete="off" /></label>
       <label class={labelCls}>Store ID (optional)<input type="text" class={inputCls + ' font-mono'} bind:value={draft.vercelBlob.store_id} /></label>
       <label class={labelCls}>Key prefix (optional)<input type="text" class={inputCls + ' font-mono'} bind:value={draft.vercelBlob.prefix} /></label>
      </div>
     {/if}
    {/if}
   </div>

   <div class="mt-4 flex items-center justify-end gap-2">
    <button
     type="button"
     class="px-3 py-1.5 text-xs rounded bg-slate-100 dark:bg-warm-900 text-slate-700 dark:text-slate-300 hover:bg-slate-200 dark:hover:bg-warm-700 cursor-pointer"
     onclick={closeEditor}
    >Cancel</button>
    <button
     type="button"
     class="px-3 py-1.5 text-xs rounded bg-accent-600 text-white font-medium hover:bg-accent-700 disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer"
     onclick={saveDraft}
     disabled={saving || !draft.prefix.trim()}
    >{saving ? 'Saving…' : 'Save'}</button>
   </div>
  </div>
 {/if}
</div>
