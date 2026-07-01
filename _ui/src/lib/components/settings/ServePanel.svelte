<script lang="ts">
 // File-serving panel — operator-facing config for kutu's built-in
 // FTP / SFTP / TFTP / WebDAV servers. The whole document (protocol
 // settings + the shared user & share lists) is edited as one draft and
 // persisted with a single Save, which reconciles the running servers
 // server-side.
 //
 // Shares point at raw-mount paths ("<mount-prefix>" or
 // "<mount-prefix>/<sub/path>"); a datalist of the configured mount
 // prefixes is offered so the operator picks valid backends. Users
 // authenticate FTP / SFTP / WebDAV (TFTP is anonymous + read-only).
 import { Server, Globe, HardDrive, Network, Plus, Trash2, Users, FolderOpen } from 'lucide-svelte';
 import type { ServeSettings, ServeStatus, ServeUser, ServeShare } from '@/lib/types/config';
 import { serveStore } from '@/lib/store/serve.svelte';
 import { rawMountsStore } from '@/lib/store/rawmounts.svelte';

 function clone(s: ServeSettings): ServeSettings {
  return structuredClone($state.snapshot(s)) as ServeSettings;
 }

 let draft = $state<ServeSettings>(clone(serveStore.settings));
 // Track the store object identity so we re-sync the draft only when the
 // store replaces it (load / save), never while the operator is typing.
 let synced = serveStore.settings;
 $effect(() => {
  if (serveStore.settings !== synced) {
   synced = serveStore.settings;
   draft = clone(serveStore.settings);
  }
 });

 const dirty = $derived(JSON.stringify(draft) !== JSON.stringify(serveStore.settings));
 const mountPrefixes = $derived(rawMountsStore.configs.map(c => c.prefix));

 function statusFor(proto: string): ServeStatus | undefined {
  return serveStore.status.find(s => s.protocol === proto);
 }

 // ── users ──
 function addUser() {
  draft.users = [...(draft.users ?? []), { username: '', password: '', shares: [], read_only: false }];
 }
 function removeUser(i: number) {
  draft.users = (draft.users ?? []).filter((_, idx) => idx !== i);
 }
 function toggleUserShare(u: ServeUser, name: string, on: boolean) {
  const cur = new Set(u.shares ?? []);
  if (on) cur.add(name); else cur.delete(name);
  u.shares = [...cur];
 }

 // ── shares ──
 function addShare() {
  draft.shares = [...(draft.shares ?? []), { name: '', paths: [''], read_only: false, root: false }];
 }
 function removeShare(i: number) {
  draft.shares = (draft.shares ?? []).filter((_, idx) => idx !== i);
 }
 function addPath(sh: ServeShare) {
  sh.paths = [...sh.paths, ''];
 }
 function removePath(sh: ServeShare, i: number) {
  sh.paths = sh.paths.filter((_, idx) => idx !== i);
 }

 async function save() {
  // Drop fully-empty path rows before sending.
  const payload = clone(draft);
  for (const sh of payload.shares ?? []) sh.paths = sh.paths.map(p => p.trim()).filter(Boolean);
  await serveStore.save(payload);
 }

 function resetDraft() {
  draft = clone(serveStore.settings);
 }

 const inputCls = 'rounded border border-slate-200 dark:border-warm-700 bg-white dark:bg-warm-900 px-2 py-1 text-xs';
 const labelCls = 'text-xs text-slate-600 dark:text-slate-300 flex flex-col gap-1';

 const protocols = [
  { key: 'ftp', label: 'FTP / FTPS', icon: Server, defPort: 2121 },
  { key: 'sftp', label: 'SFTP (SSH)', icon: Network, defPort: 2222 },
  { key: 'tftp', label: 'TFTP', icon: HardDrive, defPort: 69 },
  { key: 'webdav', label: 'WebDAV', icon: Globe, defPort: 9119 },
 ] as const;
</script>

<div>
 <header class="mb-5 flex items-start justify-between gap-4">
  <div>
   <h2 class="text-base font-semibold text-slate-800 dark:text-slate-100 flex items-center gap-2">
    <Server size={18} class="text-accent-600 dark:text-accent-400" />
    File serving
   </h2>
   <p class="text-sm text-slate-500 dark:text-slate-400 mt-1">
    Expose raw mounts over <strong>FTP</strong>, <strong>SFTP</strong>, <strong>TFTP</strong> and <strong>WebDAV</strong>.
    Shares select which mount paths are served; users provide the credentials (TFTP is anonymous and read-only).
   </p>
  </div>
  <div class="flex items-center gap-2 shrink-0">
   {#if dirty}
    <button
     type="button"
     class="px-3 py-1.5 text-xs rounded bg-slate-100 dark:bg-warm-900 text-slate-700 dark:text-slate-300 hover:bg-slate-200 dark:hover:bg-warm-700 cursor-pointer"
     onclick={resetDraft}
    >Reset</button>
   {/if}
   <button
    type="button"
    class="px-3 py-1.5 text-xs rounded bg-accent-600 text-white font-medium hover:bg-accent-700 disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer"
    onclick={save}
    disabled={serveStore.saving || !dirty}
   >{serveStore.saving ? 'Saving…' : 'Save changes'}</button>
  </div>
 </header>

 <!-- ── Protocols ── -->
 <div class="grid gap-3 md:grid-cols-2">
  {#each protocols as p (p.key)}
   {@const st = statusFor(p.key)}
   {@const Icon = p.icon}
   {@const cfg = draft[p.key]}
   <div class="bg-white dark:bg-warm-800 border border-slate-200 dark:border-warm-700 rounded-lg p-4">
    <div class="flex items-start gap-2 mb-3">
     <Icon size={16} class="text-accent-600 dark:text-accent-400 shrink-0 mt-0.5" />
     <div class="grow min-w-0">
      <div class="text-sm font-semibold text-slate-800 dark:text-slate-100">{p.label}</div>
      <div class="text-xs text-slate-500 dark:text-slate-400 font-mono truncate">
       {st?.address ?? `:${cfg.port || p.defPort}`}
      </div>
     </div>
     <span class={'shrink-0 text-[10px] uppercase tracking-wide px-1.5 py-0.5 rounded ' + (
       st?.running
        ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-300'
        : (st?.error
          ? 'bg-vermilion-100 text-vermilion-700 dark:bg-vermilion-950/40 dark:text-vermilion-300'
          : 'bg-slate-100 text-slate-600 dark:bg-warm-900 dark:text-slate-400'))}>
      {st?.running ? 'running' : (st?.error ? 'error' : 'stopped')}
     </span>
     <label class="inline-flex items-center cursor-pointer shrink-0 ml-1">
      <input type="checkbox" class="sr-only peer" bind:checked={cfg.enabled} />
      <div class="relative w-9 h-5 bg-slate-200 dark:bg-warm-700 peer-checked:bg-accent-600 rounded-full transition-colors after:content-[''] after:absolute after:top-0.5 after:left-0.5 after:bg-white after:rounded-full after:h-4 after:w-4 after:transition-all peer-checked:after:translate-x-4"></div>
     </label>
    </div>

    {#if st?.error}
     <div class="mb-3 p-2 rounded text-xs bg-vermilion-50 dark:bg-vermilion-950/30 border border-vermilion-200 dark:border-vermilion-800 text-vermilion-800 dark:text-vermilion-200 break-all">
      {st.error}
     </div>
    {/if}

    {#if cfg.enabled}
     <div class="grid grid-cols-2 gap-2">
      <label class={labelCls}>Host<input type="text" class={inputCls + ' font-mono'} bind:value={cfg.host} placeholder="0.0.0.0" /></label>
      <label class={labelCls}>Port<input type="number" class={inputCls + ' font-mono'} bind:value={cfg.port} placeholder={String(p.defPort)} /></label>

      {#if p.key === 'ftp'}
       <label class={labelCls}>Public IP (passive)<input type="text" class={inputCls + ' font-mono'} bind:value={draft.ftp.public_ip} placeholder="optional" /></label>
       <label class={labelCls}>Passive ports<input type="text" class={inputCls + ' font-mono'} bind:value={draft.ftp.passive_ports} placeholder="30000-30100" /></label>
       <label class={labelCls + ' col-span-2'}>TLS certificate (PEM, optional)<textarea class={inputCls + ' font-mono'} rows="2" bind:value={draft.ftp.tls_cert_pem} placeholder="-----BEGIN CERTIFICATE-----"></textarea></label>
       <label class={labelCls + ' col-span-2'}>TLS private key (PEM, optional)<textarea class={inputCls + ' font-mono'} rows="2" bind:value={draft.ftp.tls_key_pem} placeholder="-----BEGIN PRIVATE KEY-----"></textarea></label>
      {:else if p.key === 'sftp'}
       <label class={labelCls + ' col-span-2'}>
        Host key (PEM, optional — auto-generated &amp; persisted if empty)
        <textarea class={inputCls + ' font-mono'} rows="2" bind:value={draft.sftp.host_key_pem} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea>
       </label>
      {:else if p.key === 'tftp'}
       <p class="col-span-2 text-[11px] text-slate-400 dark:text-slate-500">
        TFTP has no authentication and is read-only. Files are fetched as <code class="font-mono">&lt;share&gt;/&lt;path&gt;</code>.
       </p>
      {:else if p.key === 'webdav'}
       <label class={labelCls + ' col-span-2'}>URL prefix<input type="text" class={inputCls + ' font-mono'} bind:value={draft.webdav.prefix} placeholder="/" /></label>
      {/if}
     </div>
    {/if}
   </div>
  {/each}
 </div>

 <!-- ── Shares ── -->
 <section class="mt-6">
  <div class="flex items-center justify-between mb-3">
   <h3 class="text-sm font-semibold text-slate-800 dark:text-slate-100 flex items-center gap-2">
    <FolderOpen size={15} class="text-accent-600 dark:text-accent-400" /> Shares
   </h3>
   <button type="button" class="px-2.5 py-1 text-xs rounded bg-accent-600 text-white font-medium hover:bg-accent-700 inline-flex items-center gap-1.5 cursor-pointer" onclick={addShare}>
    <Plus size={12} /> Add share
   </button>
  </div>

  {#if (draft.shares ?? []).length === 0}
   <p class="text-xs text-slate-400 dark:text-slate-500 border border-dashed border-slate-200 dark:border-warm-700 rounded-lg p-4 text-center">
    No shares yet. A share maps a name to one or more raw-mount paths.
   </p>
  {:else}
   <div class="flex flex-col gap-3">
    {#each draft.shares ?? [] as sh, i (i)}
     <div class="bg-white dark:bg-warm-800 border border-slate-200 dark:border-warm-700 rounded-lg p-4">
      <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
       <label class={labelCls}>Name<input type="text" class={inputCls + ' font-mono'} bind:value={sh.name} placeholder="releases" /></label>
       <div class="flex items-end gap-4">
        <label class="text-xs text-slate-600 dark:text-slate-300 inline-flex items-center gap-2"><input type="checkbox" bind:checked={sh.read_only} /> Read-only</label>
        <label class="text-xs text-slate-600 dark:text-slate-300 inline-flex items-center gap-2" title="Mount at / instead of /<name>/"><input type="checkbox" bind:checked={sh.root} /> Root mount</label>
        <button type="button" class="ml-auto px-2 py-1 rounded text-xs text-vermilion-700 dark:text-vermilion-300 hover:bg-vermilion-50 dark:hover:bg-vermilion-950/40 inline-flex items-center gap-1 cursor-pointer" onclick={() => removeShare(i)}>
         <Trash2 size={12} /> Remove
        </button>
       </div>
      </div>

      <div class="mt-3">
       <div class="text-[11px] text-slate-500 dark:text-slate-400 mb-1">Mount paths</div>
       <div class="flex flex-col gap-1.5">
        {#each sh.paths as _, pi (pi)}
         <div class="flex items-center gap-2">
          <input type="text" class={inputCls + ' font-mono grow'} bind:value={sh.paths[pi]} list="serve-mount-prefixes" placeholder="data/releases" />
          <button type="button" class="px-1.5 py-1 rounded text-vermilion-600 dark:text-vermilion-300 hover:bg-vermilion-50 dark:hover:bg-vermilion-950/40 cursor-pointer" title="Remove path" onclick={() => removePath(sh, pi)}>
           <Trash2 size={12} />
          </button>
         </div>
        {/each}
        <button type="button" class="self-start text-xs text-accent-600 dark:text-accent-400 hover:text-accent-700 dark:hover:text-accent-300 inline-flex items-center gap-1 cursor-pointer" onclick={() => addPath(sh)}>
         <Plus size={11} /> Add path
        </button>
       </div>
      </div>
     </div>
    {/each}
   </div>
  {/if}
  <datalist id="serve-mount-prefixes">
   {#each mountPrefixes as pfx (pfx)}<option value={pfx}></option>{/each}
  </datalist>
 </section>

 <!-- ── Users ── -->
 <section class="mt-6">
  <div class="flex items-center justify-between mb-3">
   <h3 class="text-sm font-semibold text-slate-800 dark:text-slate-100 flex items-center gap-2">
    <Users size={15} class="text-accent-600 dark:text-accent-400" /> Users
   </h3>
   <button type="button" class="px-2.5 py-1 text-xs rounded bg-accent-600 text-white font-medium hover:bg-accent-700 inline-flex items-center gap-1.5 cursor-pointer" onclick={addUser}>
    <Plus size={12} /> Add user
   </button>
  </div>

  {#if (draft.users ?? []).length === 0}
   <p class="text-xs text-slate-400 dark:text-slate-500 border border-dashed border-slate-200 dark:border-warm-700 rounded-lg p-4 text-center">
    No users yet. FTP / SFTP / WebDAV need at least one user to accept connections.
   </p>
  {:else}
   <div class="flex flex-col gap-3">
    {#each draft.users ?? [] as u, i (i)}
     <div class="bg-white dark:bg-warm-800 border border-slate-200 dark:border-warm-700 rounded-lg p-4">
      <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
       <label class={labelCls}>Username<input type="text" class={inputCls + ' font-mono'} bind:value={u.username} autocomplete="off" placeholder="deploy" /></label>
       <label class={labelCls}>Password<input type="password" class={inputCls + ' font-mono'} bind:value={u.password} autocomplete="new-password" placeholder="••••••••" /></label>
       <label class={labelCls + ' md:col-span-2'}>
        Authorized SSH keys (SFTP, optional — one per line)
        <textarea class={inputCls + ' font-mono'} rows="2" bind:value={u.authorized_keys} placeholder="ssh-ed25519 AAAA..."></textarea>
       </label>
      </div>

      <div class="mt-3 flex flex-wrap items-center gap-x-4 gap-y-2">
       <label class="text-xs text-slate-600 dark:text-slate-300 inline-flex items-center gap-2"><input type="checkbox" bind:checked={u.read_only} /> Read-only</label>
       <div class="flex items-center gap-2 flex-wrap">
        <span class="text-[11px] text-slate-500 dark:text-slate-400">Shares:</span>
        {#if (draft.shares ?? []).filter(s => s.name).length === 0}
         <span class="text-[11px] text-slate-400 italic">all (no shares defined)</span>
        {:else}
         {#each (draft.shares ?? []).filter(s => s.name) as s (s.name)}
          <label class="text-[11px] text-slate-600 dark:text-slate-300 inline-flex items-center gap-1 px-1.5 py-0.5 rounded border border-slate-200 dark:border-warm-700">
           <input type="checkbox" checked={(u.shares ?? []).includes(s.name)} onchange={(e) => toggleUserShare(u, s.name, e.currentTarget.checked)} />
           {s.name}
          </label>
         {/each}
         <span class="text-[10px] text-slate-400">(none checked = all)</span>
        {/if}
       </div>
       <button type="button" class="ml-auto px-2 py-1 rounded text-xs text-vermilion-700 dark:text-vermilion-300 hover:bg-vermilion-50 dark:hover:bg-vermilion-950/40 inline-flex items-center gap-1 cursor-pointer" onclick={() => removeUser(i)}>
        <Trash2 size={12} /> Remove
       </button>
      </div>
     </div>
    {/each}
   </div>
  {/if}
 </section>
</div>
