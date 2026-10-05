<script lang="ts">
 // Servers — CRUD for the file-serving server instances. Several of the
 // same protocol on different ports is fine. Each instance exposes all
 // global shares or a picked subset (none selected = all).
 //
 // Rows with inline editors; the enable switch on a row persists
 // immediately. Every save PUTs through serveStore.saveServers, which
 // only replaces the servers slice.
 import { Server, Globe, HardDrive, Network, Cloud, Plus, Trash2, LockKeyhole, Pencil } from 'lucide-svelte';
 import type { ServeServerEntry, ServeProtocol, ServeStatus } from '@/lib/types/config';
 import { serveStore } from '@/lib/store/serve.svelte';
 import { confirmAction } from '@/lib/store/confirm.svelte';
 import PanelHeader from './PanelHeader.svelte';
 import Switch from './Switch.svelte';
 import ChipPicker from './ChipPicker.svelte';

 const PROTOCOLS: { value: ServeProtocol; label: string; icon: typeof Server; defPort: number }[] = [
  { value: 'ftp', label: 'FTP / FTPS', icon: Server, defPort: 2121 },
  { value: 'sftp', label: 'SFTP (SSH)', icon: Network, defPort: 2222 },
  { value: 'tftp', label: 'TFTP', icon: HardDrive, defPort: 69 },
  { value: 'webdav', label: 'WebDAV', icon: Globe, defPort: 9119 },
  { value: 's3', label: 'S3 API', icon: Cloud, defPort: 9000 },
 ];

 const servers = $derived(serveStore.settings.servers ?? []);
 const shareNames = $derived((serveStore.settings.shares ?? []).map(s => s.name).filter(Boolean));

 // editingId tracks which server id is open. `null` collapses it;
 // "new" opens a fresh draft at the top of the list.
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
  if (editingId === sv.id) { closeEditor(); return; }
  draft = structuredClone($state.snapshot(sv)) as ServeServerEntry;
  draft.shares ??= [];
  ensureSub();
  editingId = sv.id;
 }

 function closeEditor() {
  editingId = null;
 }

 // cleanDraft keeps only the settings object of the selected protocol
 // so switching protocols in the editor never persists stale config.
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
 async function saveDraft(e?: Event) {
  e?.preventDefault();
  savingDraft = true;
  try {
   const entry = cleanDraft();
   const next = editingId === 'new'
    ? [...servers.map(s => structuredClone($state.snapshot(s)) as ServeServerEntry), entry]
    : servers.map(s => (s.id === editingId ? entry : structuredClone($state.snapshot(s)) as ServeServerEntry));
   if (await serveStore.saveServers(next)) closeEditor();
  } finally { savingDraft = false; }
 }

 // togglingId marks the row whose enable switch is being persisted.
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
  const label = sv.name || protoMeta(sv.protocol).label;
  const ok = await confirmAction({
   title: `Delete server "${label}"?`,
   message: `It stops listening on ${cardAddress(sv)} and connected clients are dropped.`,
   confirmLabel: 'Delete server',
   danger: true,
  });
  if (!ok) return;
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
  return n === 0 ? 'All shares' : (n === 1 ? sv.shares![0] : `${n} shares`);
 }
</script>

{#snippet editor()}
 {@const meta = protoMeta(draft.protocol)}
 <form class="p-4 pt-3" onsubmit={saveDraft}>
  <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
   <label class="field">
    <span class="field-label">Protocol</span>
    <select class="input" bind:value={draft.protocol} disabled={editingId !== 'new'} onchange={ensureSub}>
     {#each PROTOCOLS as p (p.value)}
      <option value={p.value}>{p.label}</option>
     {/each}
    </select>
    {#if editingId !== 'new'}<span class="field-hint">Protocol is fixed once created.</span>{/if}
   </label>
   <label class="field">
    <span class="field-label">Name <span class="font-normal text-slate-500 dark:text-warm-400">optional</span></span>
    <input type="text" class="input" bind:value={draft.name} placeholder={`${meta.label} internal`} />
   </label>
  </div>

  {#if draft[draft.protocol]}
   {@const cfg = draft[draft.protocol] as any}
   <div class="mt-4 pt-4 border-t border-dashed border-slate-300 dark:border-warm-600 grid grid-cols-1 md:grid-cols-2 gap-4">
    <label class="field"><span class="field-label">Listen host</span><input type="text" class="input font-mono" bind:value={cfg.host} placeholder="0.0.0.0" /></label>
    <label class="field"><span class="field-label">Port</span><input type="number" class="input font-mono" bind:value={cfg.port} placeholder={String(meta.defPort)} /></label>

    {#if draft.protocol === 'ftp'}
     <label class="field"><span class="field-label">Public IP for passive mode <span class="font-normal text-slate-500 dark:text-warm-400">optional</span></span><input type="text" class="input font-mono" bind:value={cfg.public_ip} /></label>
     <label class="field"><span class="field-label">Passive port range</span><input type="text" class="input font-mono" bind:value={cfg.passive_ports} placeholder="30000-30100" /></label>
     <label class="field md:col-span-2"><span class="field-label">TLS certificate <span class="font-normal text-slate-500 dark:text-warm-400">optional, PEM</span></span><textarea class="input font-mono" rows="2" bind:value={cfg.tls_cert_pem} placeholder="-----BEGIN CERTIFICATE-----"></textarea></label>
     <label class="field md:col-span-2"><span class="field-label">TLS private key <span class="font-normal text-slate-500 dark:text-warm-400">optional, PEM</span></span><textarea class="input font-mono" rows="2" bind:value={cfg.tls_key_pem} placeholder="-----BEGIN PRIVATE KEY-----"></textarea></label>
    {:else if draft.protocol === 'sftp'}
     <label class="field md:col-span-2">
      <span class="field-label">Host key <span class="font-normal text-slate-500 dark:text-warm-400">optional, PEM</span></span>
      <textarea class="input font-mono" rows="2" bind:value={cfg.host_key_pem} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea>
      <span class="field-hint">Leave empty and kutu generates one and keeps it.</span>
     </label>
    {:else if draft.protocol === 'tftp'}
     <p class="md:col-span-2 field-hint">TFTP has no authentication and is read-only. Clients fetch <code class="font-mono">&lt;share&gt;/&lt;path&gt;</code>.</p>
    {:else if draft.protocol === 'webdav' || draft.protocol === 's3'}
     <label class="field">
      <span class="field-label">Hostname <span class="font-normal text-slate-500 dark:text-warm-400">optional</span></span>
      <input type="text" class="input font-mono" bind:value={cfg.hostname} placeholder={draft.protocol === 's3' ? 's3.example.com' : 'files.example.com'} />
      <span class="field-hint">Lets S3, WebDAV and registry listeners share one port.</span>
     </label>
     {#if draft.protocol === 'webdav'}
      <label class="field"><span class="field-label">URL prefix</span><input type="text" class="input font-mono" bind:value={cfg.prefix} placeholder="/" /></label>
     {:else}
      <label class="field">
       <span class="field-label">Region</span>
       <input type="text" class="input font-mono" bind:value={cfg.region} placeholder="us-east-1" />
       <span class="field-hint">Path-style only: <code class="font-mono">host:port/&lt;share&gt;/&lt;key&gt;</code></span>
      </label>
     {/if}
     <label class="field md:col-span-2"><span class="field-label">TLS certificate <span class="font-normal text-slate-500 dark:text-warm-400">optional, PEM</span></span><textarea class="input font-mono" rows="2" bind:value={cfg.tls_cert_pem} placeholder="-----BEGIN CERTIFICATE-----"></textarea></label>
     <label class="field md:col-span-2">
      <span class="field-label">TLS private key <span class="font-normal text-slate-500 dark:text-warm-400">optional, PEM</span></span>
      <textarea class="input font-mono" rows="2" bind:value={cfg.tls_key_pem} placeholder="-----BEGIN PRIVATE KEY-----"></textarea>
      <span class="field-hint">Leave both PEM fields empty when HTTPS terminates at a reverse proxy.</span>
     </label>
    {/if}
   </div>
  {/if}

  <div class="mt-4 pt-4 border-t border-dashed border-slate-300 dark:border-warm-600">
   <ChipPicker
    label="Shares served"
    options={shareNames}
    bind:selected={draft.shares}
    emptyMeaning="Nothing selected serves every share."
    noOptions="No shares exist yet. This server will serve every share you add."
   />
  </div>

  <div class="mt-5 flex flex-wrap items-center justify-end gap-3">
   <label class="check mr-auto"><input type="checkbox" bind:checked={draft.enabled} /> Start this server</label>
   <button type="button" class="btn btn-secondary" onclick={closeEditor}>Cancel</button>
   <button type="submit" class="btn btn-primary" disabled={savingDraft || serveStore.saving}>
    {savingDraft ? 'Saving…' : editingId === 'new' ? 'Create server' : 'Save changes'}
   </button>
  </div>
 </form>
{/snippet}

<section aria-labelledby="servers-h">
 <PanelHeader title="Servers" level={2}>
  {#snippet actions()}
   <button type="button" class="btn btn-secondary btn-sm" onclick={openNew} disabled={editingId === 'new'}><Plus size={13} /> Add server</button>
  {/snippet}
 </PanelHeader>

 <div class="leaf overflow-hidden">
  {#if editingId === 'new'}
   <div class="border-b border-slate-200 dark:border-warm-700 bg-accent-50/50 dark:bg-accent-950/30">
    <div class="px-4 pt-4 text-[13px] font-semibold text-slate-900 dark:text-warm-50">New server</div>
    {@render editor()}
   </div>
  {/if}

  {#if servers.length === 0 && editingId !== 'new'}
   <div class="px-6 py-8 text-center">
    <p class="text-[13px] text-slate-600 dark:text-warm-300">No servers yet. Add one per protocol and port you want clients to reach.</p>
    <button type="button" class="btn btn-primary mt-4" onclick={openNew}><Plus size={14} /> Add server</button>
   </div>
  {:else}
   <ul class="divide-y divide-slate-200 dark:divide-warm-700">
    {#each servers as sv (sv.id)}
     {@const st = statusFor(sv.id)}
     {@const meta = protoMeta(sv.protocol)}
     {@const Icon = meta.icon}
     {@const open = editingId === sv.id}
     <li class={open ? 'bg-accent-50/50 dark:bg-accent-950/30' : ''}>
      <div class="flex flex-wrap md:flex-nowrap items-center gap-x-4 gap-y-2 px-4 py-3">
       <Switch
        checked={sv.enabled}
        disabled={togglingId === sv.id || serveStore.saving}
        label={sv.enabled ? `Stop ${sv.name || meta.label}` : `Start ${sv.name || meta.label}`}
        onchange={() => toggleEnabled(sv)}
       />
       <div class="min-w-0 flex-1 basis-48">
        <button type="button" class="flex items-center gap-2 text-left cursor-pointer group" onclick={() => openEdit(sv)} aria-expanded={open}>
         <Icon size={15} class="shrink-0 text-accent-600 dark:text-accent-300" />
         <span class="text-[14px] font-semibold text-slate-900 dark:text-warm-50 group-hover:text-accent-700 dark:group-hover:text-accent-300 truncate">{sv.name || meta.label}</span>
         {#if sv.name}<span class="tag">{sv.protocol}</span>{/if}
        </button>
        <div class="mt-0.5 font-mono text-[13px] text-slate-600 dark:text-warm-300 truncate">
         {st?.hostname ? `${st.hostname} → ${cardAddress(sv)}` : cardAddress(sv)}
        </div>
       </div>
       <div class="flex items-center gap-2 text-[13px] text-slate-600 dark:text-warm-300 md:w-36">
        {sharesSummary(sv)}{#if sv.protocol === 'tftp'}<span class="tag">read-only</span>{/if}
       </div>
       <div class="flex items-center gap-2 md:w-44">
        {#if st?.running}
         <span class="status status-ok">Running</span>
        {:else if st?.error}
         <span class="status status-err">Error</span>
        {:else}
         <span class="status status-off">Stopped</span>
        {/if}
        {#if st?.tls}<span class="tag"><LockKeyhole size={10} /> TLS</span>{/if}
        {#if st?.shared_port}<span class="tag" title="Shares its port with another listener by hostname">shared</span>{/if}
       </div>
       <div class="flex items-center gap-1 ml-auto">
        <button type="button" class="btn btn-ghost btn-sm" onclick={() => openEdit(sv)} aria-expanded={open} disabled={open}><Pencil size={13} /> Edit</button>
        <button type="button" class="btn btn-danger-ghost btn-sm btn-icon" onclick={() => deleteServer(sv)} aria-label={`Delete server ${sv.name || meta.label}`} title="Delete server"><Trash2 size={14} /></button>
       </div>
      </div>
      {#if st?.error}
       <div class="mx-4 mb-3 px-3 py-2 rounded-[3px] font-mono text-[12px] leading-relaxed break-all bg-vermilion-50 text-vermilion-800 dark:bg-vermilion-950/50 dark:text-vermilion-200">{st.error}</div>
      {/if}
      {#if open}{@render editor()}{/if}
     </li>
    {/each}
   </ul>
  {/if}
 </div>
</section>
