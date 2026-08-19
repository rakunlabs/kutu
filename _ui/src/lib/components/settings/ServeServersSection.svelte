<script lang="ts">
 // Serve servers section — CRUD for the file-serving server instances.
 // Any number of instances may exist, several of the same protocol on
 // different ports included (two S3 endpoints, an FTP per team, …).
 // Each instance picks which of the global shares it exposes (none
 // selected = all shares).
 //
 // Modelled on RawMountsPanel: a card grid of configured servers plus a
 // single inline editor drawer with explicit Save / Cancel. The enable
 // toggle on a card persists immediately. Every save PUTs the whole
 // ServeSettings document through serveStore.saveServers, which only
 // replaces the servers slice so in-flight share/user edits are never
 // dragged along.
 import { Server, Globe, HardDrive, Network, Cloud, Plus, Trash2, LockKeyhole } from 'lucide-svelte';
 import type { ServeServerEntry, ServeProtocol, ServeStatus } from '@/lib/types/config';
 import { serveStore } from '@/lib/store/serve.svelte';

 const PROTOCOLS: { value: ServeProtocol; label: string; icon: typeof Server; defPort: number }[] = [
  { value: 'ftp', label: 'FTP / FTPS', icon: Server, defPort: 2121 },
  { value: 'sftp', label: 'SFTP (SSH)', icon: Network, defPort: 2222 },
  { value: 'tftp', label: 'TFTP', icon: HardDrive, defPort: 69 },
  { value: 'webdav', label: 'WebDAV', icon: Globe, defPort: 9119 },
  { value: 's3', label: 'S3 API', icon: Cloud, defPort: 9000 },
 ];

 const servers = $derived(serveStore.settings.servers ?? []);
 const shareNames = $derived((serveStore.settings.shares ?? []).map(s => s.name).filter(Boolean));

 // editingId tracks which server id is open in the drawer. `null`
 // collapses it; "new" opens a fresh draft.
 let editingId = $state<string | null>(null);
 let draft = $state<ServeServerEntry>(emptyDraft());

 function protoMeta(p: string) {
  return PROTOCOLS.find(x => x.value === p) ?? PROTOCOLS[0];
 }

 function emptyDraft(protocol: ServeProtocol = 'ftp'): ServeServerEntry {
  return { id: '', name: '', protocol, enabled: true, shares: [], [protocol]: {} } as ServeServerEntry;
 }

 // ensureSub guarantees the settings object for the draft's protocol
 // exists so the template can bind into it without a null guard.
 function ensureSub() {
  draft[draft.protocol] ??= {};
 }

 function statusFor(id: string): ServeStatus | undefined {
  return serveStore.status.find(s => s.id === id);
 }

 function newId(protocol: string): string {
  const rnd = Math.random().toString(16).slice(2, 8);
  return `${protocol}-${rnd}`;
 }

 function openNew() {
  draft = emptyDraft();
  editingId = 'new';
 }

 function openEdit(sv: ServeServerEntry) {
  // Deep clone so editing the drawer never mutates the store row
  // until the user saves.
  draft = structuredClone($state.snapshot(sv)) as ServeServerEntry;
  draft.shares ??= [];
  ensureSub();
  editingId = sv.id;
 }

 function closeEditor() {
  editingId = null;
 }

 function toggleDraftShare(name: string, on: boolean) {
  const cur = new Set(draft.shares ?? []);
  if (on) cur.add(name); else cur.delete(name);
  draft.shares = [...cur];
 }

 // cleanDraft keeps only the settings object of the selected protocol
 // so switching protocols in the drawer never persists stale config.
 function cleanDraft(): ServeServerEntry {
  const out: ServeServerEntry = {
   id: draft.id || newId(draft.protocol),
   name: draft.name?.trim() || undefined,
   protocol: draft.protocol,
   enabled: draft.enabled,
   shares: (draft.shares ?? []).filter(s => shareNames.includes(s)),
  };
  out[draft.protocol] = structuredClone($state.snapshot(draft[draft.protocol]) ?? {}) as any;
  return out;
 }

 let savingDraft = $state(false);
 async function saveDraft() {
  savingDraft = true;
  try {
   const entry = cleanDraft();
   const next = editingId === 'new'
    ? [...servers.map(s => structuredClone($state.snapshot(s)) as ServeServerEntry), entry]
    : servers.map(s => (s.id === editingId ? entry : structuredClone($state.snapshot(s)) as ServeServerEntry));
   if (await serveStore.saveServers(next)) closeEditor();
  } finally { savingDraft = false; }
 }

 // togglingId marks the card whose enable switch is being persisted so
 // the checkbox can be disabled while the PUT is in flight.
 let togglingId = $state<string | null>(null);
 async function toggleEnabled(sv: ServeServerEntry) {
  togglingId = sv.id;
  try {
   const next = servers.map(s => {
    const copy = structuredClone($state.snapshot(s)) as ServeServerEntry;
    if (copy.id === sv.id) copy.enabled = !copy.enabled;
    return copy;
   });
   await serveStore.saveServers(next);
  } finally { togglingId = null; }
 }

 async function deleteServer(sv: ServeServerEntry) {
  const label = sv.name || `${sv.protocol} server`;
  if (!confirm(`Delete server "${label}"? Clients connected to it will be dropped.`)) return;
  const next = servers.filter(s => s.id !== sv.id).map(s => structuredClone($state.snapshot(s)) as ServeServerEntry);
  await serveStore.saveServers(next);
  if (editingId === sv.id) closeEditor();
 }

 function cardAddress(sv: ServeServerEntry): string {
  const st = statusFor(sv.id);
  if (st?.address) return st.address;
  const cfg: { host?: string; port?: number } = sv[sv.protocol] ?? {};
  return `${cfg.host || '0.0.0.0'}:${cfg.port || protoMeta(sv.protocol).defPort}`;
 }

 function sharesSummary(sv: ServeServerEntry): string {
  const n = (sv.shares ?? []).length;
  return n === 0 ? 'all shares' : (n === 1 ? sv.shares![0] : `${n} shares`);
 }

 const inputCls = 'rounded border border-slate-200 dark:border-warm-700 bg-white dark:bg-warm-900 px-2 py-1 text-xs';
 const labelCls = 'text-xs text-slate-600 dark:text-slate-300 flex flex-col gap-1';
</script>

<section>
 <div class="flex items-center justify-between mb-3">
  <h3 class="text-sm font-semibold text-slate-800 dark:text-slate-100 flex items-center gap-2">
   <Server size={15} class="text-accent-600 dark:text-accent-400" /> Servers
  </h3>
  <button
   type="button"
   class="px-2.5 py-1 text-xs rounded bg-accent-600 text-white font-medium hover:bg-accent-700 inline-flex items-center gap-1.5 cursor-pointer"
   onclick={openNew}
  >
   <Plus size={12} /> New server
  </button>
 </div>

 {#if servers.length === 0 && editingId !== 'new'}
  <div class="flex flex-col items-center justify-center py-12 px-6 text-center text-slate-400 dark:text-slate-500 border border-dashed border-slate-200 dark:border-warm-700 rounded-lg">
   <Server size={26} class="mb-3 opacity-40" />
   <div class="text-sm font-medium text-slate-600 dark:text-slate-300 mb-1">No servers configured yet</div>
   <div class="text-xs mb-4 max-w-md">
    Add as many servers as you need — several of the same protocol on different ports is fine.
    Each server exposes all shares or the subset you pick for it.
   </div>
   <button
    type="button"
    class="px-3 py-1.5 text-xs rounded bg-accent-600 text-white font-medium hover:bg-accent-700 inline-flex items-center gap-1.5 cursor-pointer"
    onclick={openNew}
   >
    <Plus size={12} /> New server
   </button>
  </div>
 {:else}
  <div class="grid gap-3 md:grid-cols-2">
   {#each servers as sv (sv.id)}
    {@const st = statusFor(sv.id)}
    {@const meta = protoMeta(sv.protocol)}
    {@const Icon = meta.icon}
    <div class="bg-white dark:bg-warm-800 border border-slate-200 dark:border-warm-700 rounded-lg p-4 flex flex-col">
     <div class="flex items-start gap-2">
      <Icon size={16} class="text-accent-600 dark:text-accent-400 shrink-0 mt-0.5" />
      <div class="grow min-w-0">
       <button
        type="button"
        class="text-sm font-semibold text-slate-800 dark:text-slate-100 hover:text-accent-600 dark:hover:text-accent-400 truncate text-left cursor-pointer"
        onclick={() => openEdit(sv)}
       >
        {sv.name || meta.label}
       </button>
        <div class="text-xs text-slate-500 dark:text-slate-400 font-mono truncate">
         {meta.label} · {st?.hostname ? `${st.hostname} → ${cardAddress(sv)}` : cardAddress(sv)}
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
      {#if st?.shared_port}
       <span class="shrink-0 text-[10px] px-1.5 py-0.5 rounded bg-accent-50 text-accent-700 dark:bg-accent-950/30 dark:text-accent-300">shared</span>
      {/if}
      {#if st?.tls}
       <span class="shrink-0 inline-flex items-center gap-1 text-[10px] px-1.5 py-0.5 rounded bg-slate-100 text-slate-600 dark:bg-warm-900 dark:text-slate-300"><LockKeyhole size={9} /> TLS</span>
      {/if}
      <label class="inline-flex items-center cursor-pointer shrink-0 ml-1" title={sv.enabled ? 'Disable server' : 'Enable server'}>
       <input
        type="checkbox"
        class="sr-only peer"
        checked={sv.enabled}
        disabled={togglingId === sv.id || serveStore.saving}
        onchange={() => toggleEnabled(sv)}
       />
       <div class="relative w-9 h-5 bg-slate-200 dark:bg-warm-700 peer-checked:bg-accent-600 rounded-full transition-colors after:content-[''] after:absolute after:top-0.5 after:left-0.5 after:bg-white after:rounded-full after:h-4 after:w-4 after:transition-all peer-checked:after:translate-x-4"></div>
      </label>
     </div>

     {#if st?.error}
      <div class="mt-3 p-2 rounded text-xs bg-vermilion-50 dark:bg-vermilion-950/30 border border-vermilion-200 dark:border-vermilion-800 text-vermilion-800 dark:text-vermilion-200 break-all">
       {st.error}
      </div>
     {/if}

     <div class="mt-3 text-[11px] text-slate-500 dark:text-slate-400">
      Shares: <span class="font-mono">{sharesSummary(sv)}</span>
      {#if sv.protocol === 'tftp'}<span class="ml-2 italic">anonymous · read-only</span>{/if}
     </div>

     <div class="mt-auto flex items-center justify-between gap-2 pt-2">
      <button
       type="button"
       class="text-xs text-accent-600 dark:text-accent-400 hover:text-accent-700 dark:hover:text-accent-300 cursor-pointer"
       onclick={() => openEdit(sv)}
      >Edit</button>
      <button
       type="button"
       class="px-2 py-1 rounded text-xs text-vermilion-700 dark:text-vermilion-300 hover:bg-vermilion-50 dark:hover:bg-vermilion-950/40 inline-flex items-center gap-1 cursor-pointer"
       title="Delete server"
       onclick={() => deleteServer(sv)}
      >
       <Trash2 size={12} />
      </button>
     </div>
    </div>
   {/each}
  </div>
 {/if}

 {#if editingId !== null}
  {@const meta = protoMeta(draft.protocol)}
  <div class="mt-4 bg-white dark:bg-warm-800 border border-slate-200 dark:border-warm-700 rounded-lg p-5">
   <h4 class="text-sm font-semibold text-slate-800 dark:text-slate-100 mb-3">
    {editingId === 'new' ? 'New server' : `Edit "${draft.name || meta.label}"`}
   </h4>

   <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
    <label class={labelCls}>
     Protocol
     <select class={inputCls} bind:value={draft.protocol} disabled={editingId !== 'new'} onchange={ensureSub}>
      {#each PROTOCOLS as p (p.value)}
       <option value={p.value}>{p.label}</option>
      {/each}
     </select>
    </label>
    <label class={labelCls}>
     Name (optional)
     <input type="text" class={inputCls} bind:value={draft.name} placeholder={`e.g. ${meta.label} internal`} />
    </label>
   </div>

   <!-- ── Bind + protocol-specific settings ── -->
   {#if draft[draft.protocol]}
    {@const cfg = draft[draft.protocol] as any}
    <div class="mt-3 rounded border border-slate-200 dark:border-warm-700 p-3 bg-slate-50/60 dark:bg-warm-900/40">
     <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
      <label class={labelCls}>Host<input type="text" class={inputCls + ' font-mono'} bind:value={cfg.host} placeholder="0.0.0.0" /></label>
      <label class={labelCls}>Port<input type="number" class={inputCls + ' font-mono'} bind:value={cfg.port} placeholder={String(meta.defPort)} /></label>

      {#if draft.protocol === 'ftp'}
       <label class={labelCls}>Public IP (passive)<input type="text" class={inputCls + ' font-mono'} bind:value={cfg.public_ip} placeholder="optional" /></label>
       <label class={labelCls}>Passive ports<input type="text" class={inputCls + ' font-mono'} bind:value={cfg.passive_ports} placeholder="30000-30100" /></label>
       <label class={labelCls + ' md:col-span-2'}>TLS certificate (PEM, optional)<textarea class={inputCls + ' font-mono'} rows="2" bind:value={cfg.tls_cert_pem} placeholder="-----BEGIN CERTIFICATE-----"></textarea></label>
       <label class={labelCls + ' md:col-span-2'}>TLS private key (PEM, optional)<textarea class={inputCls + ' font-mono'} rows="2" bind:value={cfg.tls_key_pem} placeholder="-----BEGIN PRIVATE KEY-----"></textarea></label>
      {:else if draft.protocol === 'sftp'}
       <label class={labelCls + ' md:col-span-2'}>
        Host key (PEM, optional — auto-generated &amp; persisted if empty)
        <textarea class={inputCls + ' font-mono'} rows="2" bind:value={cfg.host_key_pem} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea>
       </label>
      {:else if draft.protocol === 'tftp'}
       <p class="md:col-span-2 text-[11px] text-slate-400 dark:text-slate-500">
        TFTP has no authentication and is read-only. Files are fetched as <code class="font-mono">&lt;share&gt;/&lt;path&gt;</code>.
       </p>
      {:else if draft.protocol === 'webdav'}
        <label class={labelCls}>Hostname (optional)<input type="text" class={inputCls + ' font-mono'} bind:value={cfg.hostname} placeholder="files.example.com" /></label>
        <label class={labelCls + ' md:col-span-2'}>URL prefix<input type="text" class={inputCls + ' font-mono'} bind:value={cfg.prefix} placeholder="/" /></label>
        <label class={labelCls + ' md:col-span-2'}>TLS certificate (PEM, optional)<textarea class={inputCls + ' font-mono'} rows="2" bind:value={cfg.tls_cert_pem} placeholder="Leave blank behind a TLS-terminating reverse proxy"></textarea></label>
        <label class={labelCls + ' md:col-span-2'}>TLS private key (PEM, optional)<textarea class={inputCls + ' font-mono'} rows="2" bind:value={cfg.tls_key_pem} placeholder="Both certificate and key are required for direct TLS"></textarea></label>
       {:else if draft.protocol === 's3'}
        <label class={labelCls}>Hostname (optional)<input type="text" class={inputCls + ' font-mono'} bind:value={cfg.hostname} placeholder="s3.example.com" /></label>
        <label class={labelCls}>Region<input type="text" class={inputCls + ' font-mono'} bind:value={cfg.region} placeholder="us-east-1" /></label>
       <p class="text-[11px] text-slate-400 dark:text-slate-500 self-end pb-1">
        Path-style only: <code class="font-mono">http://host:port/&lt;share&gt;/&lt;key&gt;</code>
       </p>
       <label class={labelCls + ' md:col-span-2'}>TLS certificate (PEM, optional)<textarea class={inputCls + ' font-mono'} rows="2" bind:value={cfg.tls_cert_pem} placeholder="-----BEGIN CERTIFICATE-----"></textarea></label>
        <label class={labelCls + ' md:col-span-2'}>TLS private key (PEM, optional)<textarea class={inputCls + ' font-mono'} rows="2" bind:value={cfg.tls_key_pem} placeholder="-----BEGIN PRIVATE KEY-----"></textarea></label>
       {/if}
       {#if draft.protocol === 'webdav' || draft.protocol === 's3'}
        <p class="md:col-span-2 text-[11px] leading-relaxed text-slate-400 dark:text-slate-500">
         S3, WebDAV and registry listeners can share a port when each has a different hostname.
         TLS is optional; leave both PEM fields blank when HTTPS terminates at a reverse proxy.
        </p>
       {/if}
     </div>
    </div>
   {/if}

   <!-- ── Share assignment ── -->
   <div class="mt-3">
    <div class="text-[11px] text-slate-500 dark:text-slate-400 mb-1.5">
     Shares served by this instance <span class="text-slate-400">(none checked = all)</span>
    </div>
    {#if shareNames.length === 0}
     <p class="text-[11px] text-slate-400 italic">No shares defined yet — this server will expose every share you add below.</p>
    {:else}
     <div class="flex flex-wrap gap-1.5">
      {#each shareNames as name (name)}
       <label class="text-[11px] text-slate-600 dark:text-slate-300 inline-flex items-center gap-1 px-1.5 py-0.5 rounded border border-slate-200 dark:border-warm-700 cursor-pointer">
        <input
         type="checkbox"
         checked={(draft.shares ?? []).includes(name)}
         onchange={(e) => toggleDraftShare(name, e.currentTarget.checked)}
        />
        {name}
       </label>
      {/each}
     </div>
    {/if}
   </div>

   <div class="mt-4 flex items-center justify-between gap-2">
    <label class="text-xs text-slate-600 dark:text-slate-300 inline-flex items-center gap-2 cursor-pointer">
     <input type="checkbox" bind:checked={draft.enabled} /> Enabled
    </label>
    <div class="flex items-center gap-2">
     <button
      type="button"
      class="px-3 py-1.5 text-xs rounded bg-slate-100 dark:bg-warm-900 text-slate-700 dark:text-slate-300 hover:bg-slate-200 dark:hover:bg-warm-700 cursor-pointer"
      onclick={closeEditor}
     >Cancel</button>
     <button
      type="button"
      class="px-3 py-1.5 text-xs rounded bg-accent-600 text-white font-medium hover:bg-accent-700 disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer"
      onclick={saveDraft}
      disabled={savingDraft || serveStore.saving}
     >{savingDraft ? 'Saving…' : 'Save server'}</button>
    </div>
   </div>
  </div>
 {/if}
</section>
