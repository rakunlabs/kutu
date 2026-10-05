<script lang="ts">
 // Raw mounts — CRUD for the storage backends the file browser,
 // registries and proxy raw handler all read from. A raw mount maps a
 // single prefix (/raw/<prefix>/...) to one of six backends.
 //
 // Rows come from the persisted `configs` slice (so a mount shows even
 // when its backend is momentarily unreachable); the `mounts` slice
 // supplies the live writable / inactive state. Editing happens inline,
 // directly under the row being edited.
 import type { RawMount, RawMountConfig, RawMountType } from '@/lib/types/config';
 import { rawMountsStore } from '@/lib/store/rawmounts.svelte';
 import { confirmAction } from '@/lib/store/confirm.svelte';
 import { HardDrive, Plus, Trash2, Cloud, Server, Folder, Globe, Database, Pencil } from 'lucide-svelte';
 import PanelHeader from './PanelHeader.svelte';

 let {
  configs,
  mounts,
 }: {
  configs: RawMountConfig[];
  mounts: RawMount[];
 } = $props();

 // editingId tracks which mount prefix is open in the editor. `null`
 // collapses it; "new" opens a fresh draft at the top of the list.
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

 function typeLabel(type?: string): string {
  return TYPES.find(t => t.value === (type ?? 'local'))?.label ?? type ?? 'Local directory';
 }

 // target is the one line that tells an operator where the bytes live.
 function target(cfg: RawMountConfig): string {
  switch (cfg.type ?? 'local') {
   case 's3': return [cfg.s3?.endpoint?.replace(/^https?:\/\//, ''), cfg.s3?.bucket, cfg.s3?.prefix].filter(Boolean).join(' / ') || '—';
   case 'ftp': return `${cfg.ftp?.host ?? ''}${cfg.ftp?.base_path ?? ''}` || '—';
   case 'sftp': return `${cfg.sftp?.host ?? ''}${cfg.sftp?.base_path ?? ''}` || '—';
   case 'webdav': return cfg.webdav?.url ?? '—';
   case 'vercel-blob': return cfg.vercelBlob?.store_id || cfg.vercelBlob?.prefix || 'Vercel Blob store';
   default: return cfg.path || '—';
  }
 }

 function openNew() {
  draft = emptyDraft();
  editingId = 'new';
 }

 function openEdit(cfg: RawMountConfig) {
  if (editingId === cfg.prefix) { closeEditor(); return; }
  // Deep clone so editing never mutates the store row until saved.
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

 const prefixProblem = $derived.by(() => {
  const p = draft.prefix.trim();
  if (!p) return 'Prefix is required.';
  if (/[/\\\s]/.test(p)) return 'Use a single path segment: no slashes or spaces.';
  if (editingId === 'new' && configs.some(c => c.prefix === p)) return `"${p}" is already used by another mount.`;
  return null;
 });

 async function saveDraft(e?: Event) {
  e?.preventDefault();
  if (prefixProblem) return;
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
  const ok = await confirmAction({
   title: `Delete raw mount "${cfg.prefix}"?`,
   message: 'Repositories, shares or proxies pointing at this prefix will stop resolving. Files on the backend itself are not touched.',
   confirmLabel: 'Delete mount',
   danger: true,
  });
  if (!ok) return;
  try {
   await rawMountsStore.remove(cfg.prefix);
   if (editingId === cfg.prefix) closeEditor();
  } catch {/* toast in store */}
 }
</script>

{#snippet editor()}
 <form class="editor" onsubmit={saveDraft}>
  <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
   <label class="field">
    <span class="field-label">Prefix</span>
    <input
     type="text"
     class="input font-mono"
     bind:value={draft.prefix}
     disabled={editingId !== 'new'}
     placeholder="data"
     aria-invalid={editingId === 'new' && !!draft.prefix && !!prefixProblem}
    />
    <span class="field-hint">Served at <code class="font-mono">/raw/{draft.prefix.trim() || '<prefix>'}</code>{editingId !== 'new' ? ' · cannot be renamed' : ''}</span>
   </label>

   <label class="field">
    <span class="field-label">Backend</span>
    <select class="input" bind:value={draft.type} onchange={ensureSub}>
     {#each TYPES as t (t.value)}
      <option value={t.value}>{t.label}</option>
     {/each}
    </select>
   </label>
  </div>

  <div class="mt-4 pt-4 border-t border-dashed border-slate-300 dark:border-warm-600">
   {#if draft.type === 'local' || !draft.type}
    <label class="field">
     <span class="field-label">Directory path</span>
     <input type="text" class="input font-mono" bind:value={draft.path} placeholder="/var/lib/kutu/data" />
     <span class="field-hint">Must already exist on the server and be a directory.</span>
    </label>
   {:else if draft.type === 's3' && draft.s3}
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
     <label class="field"><span class="field-label">Bucket</span><input type="text" class="input font-mono" bind:value={draft.s3.bucket} placeholder="my-bucket" /></label>
     <label class="field"><span class="field-label">Region</span><input type="text" class="input font-mono" bind:value={draft.s3.region} placeholder="us-east-1" /></label>
     <label class="field md:col-span-2"><span class="field-label">Endpoint</span><input type="text" class="input font-mono" bind:value={draft.s3.endpoint} placeholder="https://minio.example.com" /><span class="field-hint">Leave empty for AWS S3.</span></label>
     <label class="field"><span class="field-label">Access key</span><input type="text" class="input font-mono" bind:value={draft.s3.access_key} autocomplete="off" /></label>
     <label class="field"><span class="field-label">Secret key</span><input type="password" class="input font-mono" bind:value={draft.s3.secret_key} autocomplete="off" /></label>
     <label class="field"><span class="field-label">Key prefix <span class="font-normal text-slate-500 dark:text-warm-400">optional</span></span><input type="text" class="input font-mono" bind:value={draft.s3.prefix} placeholder="artifacts/" /></label>
     <div class="flex items-end gap-5 pb-1.5">
      <label class="check"><input type="checkbox" bind:checked={draft.s3.path_style} /> Path-style URLs</label>
      <label class="check"><input type="checkbox" bind:checked={draft.s3.secure} /> HTTPS</label>
     </div>
    </div>
   {:else if draft.type === 'ftp' && draft.ftp}
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
     <label class="field"><span class="field-label">Host</span><input type="text" class="input font-mono" bind:value={draft.ftp.host} placeholder="ftp.example.com:21" /></label>
     <label class="field"><span class="field-label">Base path <span class="font-normal text-slate-500 dark:text-warm-400">optional</span></span><input type="text" class="input font-mono" bind:value={draft.ftp.base_path} placeholder="/pub" /></label>
     <label class="field"><span class="field-label">Username</span><input type="text" class="input font-mono" bind:value={draft.ftp.username} autocomplete="off" /></label>
     <label class="field"><span class="field-label">Password</span><input type="password" class="input font-mono" bind:value={draft.ftp.password} autocomplete="off" /></label>
     <label class="check md:col-span-2"><input type="checkbox" bind:checked={draft.ftp.tls} /> Use explicit TLS (FTPS)</label>
    </div>
   {:else if draft.type === 'sftp' && draft.sftp}
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
     <label class="field"><span class="field-label">Host</span><input type="text" class="input font-mono" bind:value={draft.sftp.host} placeholder="sftp.example.com:22" /></label>
     <label class="field"><span class="field-label">Base path <span class="font-normal text-slate-500 dark:text-warm-400">optional</span></span><input type="text" class="input font-mono" bind:value={draft.sftp.base_path} placeholder="/upload" /></label>
     <label class="field"><span class="field-label">Username</span><input type="text" class="input font-mono" bind:value={draft.sftp.username} autocomplete="off" /></label>
     <label class="field"><span class="field-label">Password</span><input type="password" class="input font-mono" bind:value={draft.sftp.password} autocomplete="off" /></label>
     <label class="field md:col-span-2"><span class="field-label">Private key <span class="font-normal text-slate-500 dark:text-warm-400">optional, PEM</span></span><textarea class="input font-mono" rows="3" bind:value={draft.sftp.private_key} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea><span class="field-hint">When set, the key is used instead of the password.</span></label>
    </div>
   {:else if draft.type === 'webdav' && draft.webdav}
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
     <label class="field md:col-span-2"><span class="field-label">URL</span><input type="text" class="input font-mono" bind:value={draft.webdav.url} placeholder="https://dav.example.com/remote.php/dav" /></label>
     <label class="field"><span class="field-label">Username</span><input type="text" class="input font-mono" bind:value={draft.webdav.username} autocomplete="off" /></label>
     <label class="field"><span class="field-label">Password</span><input type="password" class="input font-mono" bind:value={draft.webdav.password} autocomplete="off" /></label>
     <label class="field md:col-span-2"><span class="field-label">Base path <span class="font-normal text-slate-500 dark:text-warm-400">optional</span></span><input type="text" class="input font-mono" bind:value={draft.webdav.base_path} placeholder="/files" /></label>
    </div>
   {:else if draft.type === 'vercel-blob' && draft.vercelBlob}
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
     <label class="field md:col-span-2"><span class="field-label">Token</span><input type="password" class="input font-mono" bind:value={draft.vercelBlob.token} placeholder="vercel_blob_rw_..." autocomplete="off" /></label>
     <label class="field"><span class="field-label">Store ID <span class="font-normal text-slate-500 dark:text-warm-400">optional</span></span><input type="text" class="input font-mono" bind:value={draft.vercelBlob.store_id} /></label>
     <label class="field"><span class="field-label">Key prefix <span class="font-normal text-slate-500 dark:text-warm-400">optional</span></span><input type="text" class="input font-mono" bind:value={draft.vercelBlob.prefix} /></label>
    </div>
   {/if}
  </div>

  <div class="mt-5 flex flex-wrap items-center justify-end gap-3">
   {#if editingId === 'new' && draft.prefix && prefixProblem}
    <span class="mr-auto text-[13px] text-vermilion-700 dark:text-vermilion-300">{prefixProblem}</span>
   {/if}
   <button type="button" class="btn btn-secondary" onclick={closeEditor}>Cancel</button>
   <button type="submit" class="btn btn-primary" disabled={saving || !!prefixProblem}>
    {saving ? 'Saving…' : editingId === 'new' ? 'Create mount' : 'Save changes'}
   </button>
  </div>
 </form>
{/snippet}

<PanelHeader title="Raw mounts">
 Storage backends served under <code class="font-mono text-[13px]">/raw/&lt;prefix&gt;</code>. The Files page browses them; registries and file shares point at them.
 {#snippet actions()}
  {#if configs.length > 0}
   <button type="button" class="btn btn-primary" onclick={openNew} disabled={editingId === 'new'}>
    <Plus size={14} /> Add mount
   </button>
  {/if}
 {/snippet}
</PanelHeader>

{#if configs.length === 0 && editingId !== 'new'}
 <div class="leaf border-dashed px-6 py-12 text-center">
  <HardDrive size={28} class="mx-auto mb-3 text-accent-500" />
  <h2 class="text-[15px] font-semibold text-slate-900 dark:text-warm-50">Connect your first storage backend</h2>
  <p class="mt-1.5 mx-auto max-w-md text-[13px] leading-relaxed text-slate-600 dark:text-warm-300">
   A mount gives a prefix to a local directory, S3 bucket, FTP, SFTP, WebDAV or Vercel Blob store.
   Registries and the file browser can only use prefixes defined here.
  </p>
  <button type="button" class="btn btn-primary mt-5" onclick={openNew}><Plus size={14} /> Add mount</button>
 </div>
{:else}
 <div class="leaf overflow-hidden">
  <div class="cols thead label-caps px-4 py-2 text-[11px] text-slate-500 dark:text-warm-400 border-b border-slate-200 dark:border-warm-700 bg-slate-50 dark:bg-warm-900/50">
   <span></span><span>Prefix</span><span>Backend</span><span>Location</span><span>State</span><span></span>
  </div>

  {#if editingId === 'new'}
   <div class="border-b border-slate-200 dark:border-warm-700 bg-accent-50/50 dark:bg-accent-950/30">
    <div class="px-4 pt-4 text-[13px] font-semibold text-slate-900 dark:text-warm-50">New raw mount</div>
    {@render editor()}
   </div>
  {/if}

  <ul class="divide-y divide-slate-200 dark:divide-warm-700">
   {#each configs as cfg (cfg.prefix)}
    {@const sum = summaryFor(cfg.prefix)}
    {@const Icon = typeIcon(cfg.type)}
    {@const open = editingId === cfg.prefix}
    <li class={open ? 'bg-accent-50/50 dark:bg-accent-950/30' : ''}>
     <div class="row cols px-4 py-3">
      <Icon size={16} class="text-accent-600 dark:text-accent-300 hidden md:block" />
      <button
       type="button"
       class="text-left font-mono text-[14px] font-semibold text-slate-900 dark:text-warm-50 hover:text-accent-700 dark:hover:text-accent-300 truncate cursor-pointer"
       onclick={() => openEdit(cfg)}
       aria-expanded={open}
      >{cfg.prefix}</button>
      <span class="text-[13px] text-slate-600 dark:text-warm-300 truncate">{typeLabel(cfg.type)}</span>
      <span class="font-mono text-[13px] text-slate-600 dark:text-warm-300 truncate" title={target(cfg)}>{target(cfg)}</span>
      <span>
       {#if !sum}
        <span class="status status-warn" title="Backend is not active: check the configuration or that the target is reachable">Unreachable</span>
       {:else if sum.writable}
        <span class="status status-ok">Writable</span>
       {:else}
        <span class="status status-off">Read-only</span>
       {/if}
      </span>
      <span class="flex items-center justify-end gap-1">
       <button type="button" class="btn btn-ghost btn-sm" onclick={() => openEdit(cfg)} aria-expanded={open} disabled={open}>
        <Pencil size={13} /> Edit
       </button>
       <button type="button" class="btn btn-danger-ghost btn-sm btn-icon" onclick={() => deleteMount(cfg)} aria-label={`Delete raw mount ${cfg.prefix}`} title="Delete mount">
        <Trash2 size={14} />
       </button>
      </span>
     </div>
     {#if open}{@render editor()}{/if}
    </li>
   {/each}
  </ul>
 </div>
{/if}

<style>
 .cols {
  display: grid;
  align-items: center;
  column-gap: 1rem;
  row-gap: 0.25rem;
  grid-template-columns: minmax(0, 1fr) auto;
  grid-template-areas:
   'name actions'
   'type state'
   'loc loc';
 }

 .row > :nth-child(1) { display: none; }
 .row > :nth-child(2) { grid-area: name; }
 .row > :nth-child(3) { grid-area: type; }
 .row > :nth-child(4) { grid-area: loc; }
 .row > :nth-child(5) { grid-area: state; justify-self: end; }
 .row > :nth-child(6) { grid-area: actions; }

 @media (min-width: 768px) {
  .cols {
   grid-template-columns: 1rem minmax(6rem, 0.8fr) minmax(7rem, 0.9fr) minmax(0, 2fr) 7rem auto;
   grid-template-areas: none;
  }

  .row > :nth-child(n) { grid-area: auto; justify-self: stretch; }
  .row > :nth-child(1) { display: block; }
 }

 .cols.thead { display: none; }

 @media (min-width: 768px) {
  .cols.thead { display: grid; }
 }

 .editor {
  padding: 1rem;
  padding-top: 0.75rem;
 }
</style>
