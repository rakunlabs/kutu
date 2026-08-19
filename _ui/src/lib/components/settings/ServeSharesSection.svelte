<script lang="ts">
 // Serve shares section — CRUD for the global share pool. A share maps
 // a name to one or more raw-mount paths ("<mount-prefix>" or
 // "<mount-prefix>/<sub/path>"); server instances then expose all
 // shares or a named subset.
 //
 // Same card grid + inline editor drawer pattern as the servers
 // section. Renaming or deleting a share also prunes stale references
 // from servers and users in the same PUT so the backend validation
 // never trips over a dangling name.
 import { FolderOpen, Plus, Trash2 } from 'lucide-svelte';
 import type { ServeShare, ServeServerEntry, ServeUser } from '@/lib/types/config';
 import { serveStore } from '@/lib/store/serve.svelte';
 import { rawMountsStore } from '@/lib/store/rawmounts.svelte';

 const shares = $derived(serveStore.settings.shares ?? []);
 const mountPrefixes = $derived(rawMountsStore.configs.map(c => c.prefix));

 // Which servers expose a given share (explicitly or via "all").
 function usedBy(name: string): string {
  const servers = serveStore.settings.servers ?? [];
  const explicit = servers.filter(s => (s.shares ?? []).includes(name));
  const implicit = servers.filter(s => (s.shares ?? []).length === 0);
  const n = explicit.length + implicit.length;
  if (servers.length === 0 || n === 0) return 'no servers';
  if (n === servers.length) return 'all servers';
  return `${n} server${n === 1 ? '' : 's'}`;
 }

 // editingIndex tracks which share row is open in the drawer. `null`
 // collapses it; -1 opens a fresh draft.
 let editingIndex = $state<number | null>(null);
 let draft = $state<ServeShare>(emptyDraft());
 let savingDraft = $state(false);

 function emptyDraft(): ServeShare {
  return { name: '', paths: [''], read_only: false, root: false };
 }

 function openNew() {
  draft = emptyDraft();
  editingIndex = -1;
 }

 function openEdit(i: number) {
  draft = structuredClone($state.snapshot(shares[i])) as ServeShare;
  if (draft.paths.length === 0) draft.paths = [''];
  editingIndex = i;
 }

 function closeEditor() {
  editingIndex = null;
 }

 function addPath() {
  draft.paths = [...draft.paths, ''];
 }

 function removePath(i: number) {
  draft.paths = draft.paths.filter((_, idx) => idx !== i);
 }

 const draftValid = $derived(
  draft.name.trim() !== ''
  && !/[/\\]/.test(draft.name)
  && draft.paths.some(p => p.trim() !== '')
 );

 // fullDocSave persists a new shares slice and prunes references to
 // share names that no longer exist from servers and users.
 async function fullDocSave(nextShares: ServeShare[]): Promise<boolean> {
  const doc = structuredClone($state.snapshot(serveStore.settings)) as typeof serveStore.settings;
  const names = new Set(nextShares.map(s => s.name));
  doc.shares = nextShares;
  doc.servers = (doc.servers ?? []).map((s: ServeServerEntry) => ({
   ...s, shares: (s.shares ?? []).filter(n => names.has(n)),
  }));
  doc.users = (doc.users ?? []).map((u: ServeUser) => ({
   ...u, shares: (u.shares ?? []).filter(n => names.has(n)),
  }));
  return serveStore.save(doc);
 }

 async function saveDraft() {
  savingDraft = true;
  try {
   const entry = structuredClone($state.snapshot(draft)) as ServeShare;
   entry.name = entry.name.trim();
   entry.paths = entry.paths.map(p => p.trim()).filter(Boolean);
   const next = shares.map(s => structuredClone($state.snapshot(s)) as ServeShare);
   if (editingIndex === -1) next.push(entry);
   else next[editingIndex!] = entry;
   if (await fullDocSave(next)) closeEditor();
  } finally { savingDraft = false; }
 }

 async function deleteShare(i: number) {
  const sh = shares[i];
  if (!confirm(`Delete share "${sh.name}"? Servers and users referencing it will lose access to it.`)) return;
  const next = shares.filter((_, idx) => idx !== i).map(s => structuredClone($state.snapshot(s)) as ServeShare);
  await fullDocSave(next);
  if (editingIndex === i) closeEditor();
 }

 const inputCls = 'rounded border border-slate-200 dark:border-warm-700 bg-white dark:bg-warm-900 px-2 py-1 text-xs';
 const labelCls = 'text-xs text-slate-600 dark:text-slate-300 flex flex-col gap-1';
</script>

<section class="mt-8">
 <div class="flex items-center justify-between mb-3">
  <h3 class="text-sm font-semibold text-slate-800 dark:text-slate-100 flex items-center gap-2">
   <FolderOpen size={15} class="text-accent-600 dark:text-accent-400" /> Shares
  </h3>
  <button
   type="button"
   class="px-2.5 py-1 text-xs rounded bg-accent-600 text-white font-medium hover:bg-accent-700 inline-flex items-center gap-1.5 cursor-pointer"
   onclick={openNew}
  >
   <Plus size={12} /> New share
  </button>
 </div>

 {#if shares.length === 0 && editingIndex !== -1}
  <p class="text-xs text-slate-400 dark:text-slate-500 border border-dashed border-slate-200 dark:border-warm-700 rounded-lg p-4 text-center">
   No shares yet. A share maps a name to one or more raw-mount paths; assign it to specific servers from their editor.
  </p>
 {:else}
  <div class="grid gap-3 md:grid-cols-2 lg:grid-cols-3">
   {#each shares as sh, i (sh.name + '\u0000' + i)}
    <div class="bg-white dark:bg-warm-800 border border-slate-200 dark:border-warm-700 rounded-lg p-4 flex flex-col">
     <div class="flex items-start gap-2 mb-1">
      <FolderOpen size={15} class="text-accent-600 dark:text-accent-400 shrink-0 mt-0.5" />
      <div class="grow min-w-0">
       <button
        type="button"
        class="text-sm font-semibold text-slate-800 dark:text-slate-100 hover:text-accent-600 dark:hover:text-accent-400 truncate text-left cursor-pointer font-mono"
        onclick={() => openEdit(i)}
       >
        {sh.name}
       </button>
       <div class="text-xs text-slate-500 dark:text-slate-400 font-mono truncate" title={sh.paths.join(', ')}>
        {sh.paths.join(', ') || '—'}
       </div>
      </div>
     </div>

     <div class="flex flex-wrap items-center gap-1.5 mt-1 text-[10px]">
      {#if sh.read_only}
       <span class="uppercase tracking-wide px-1.5 py-0.5 rounded bg-slate-100 text-slate-600 dark:bg-warm-900 dark:text-slate-400">read-only</span>
      {/if}
      {#if sh.root}
       <span class="uppercase tracking-wide px-1.5 py-0.5 rounded bg-amber-100 text-amber-700 dark:bg-amber-950/40 dark:text-amber-300">root mount</span>
      {/if}
      <span class="text-slate-400 dark:text-slate-500">{usedBy(sh.name)}</span>
     </div>

     <div class="mt-auto flex items-center justify-between gap-2 pt-2">
      <button
       type="button"
       class="text-xs text-accent-600 dark:text-accent-400 hover:text-accent-700 dark:hover:text-accent-300 cursor-pointer"
       onclick={() => openEdit(i)}
      >Edit</button>
      <button
       type="button"
       class="px-2 py-1 rounded text-xs text-vermilion-700 dark:text-vermilion-300 hover:bg-vermilion-50 dark:hover:bg-vermilion-950/40 inline-flex items-center gap-1 cursor-pointer"
       title="Delete share"
       onclick={() => deleteShare(i)}
      >
       <Trash2 size={12} />
      </button>
     </div>
    </div>
   {/each}
  </div>
 {/if}

 {#if editingIndex !== null}
  <div class="mt-4 bg-white dark:bg-warm-800 border border-slate-200 dark:border-warm-700 rounded-lg p-5">
   <h4 class="text-sm font-semibold text-slate-800 dark:text-slate-100 mb-3">
    {editingIndex === -1 ? 'New share' : `Edit "${shares[editingIndex]?.name}"`}
   </h4>

   <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
    <label class={labelCls}>
     Name
     <input type="text" class={inputCls + ' font-mono'} bind:value={draft.name} placeholder="releases" />
     <span class="text-[10px] text-slate-400">Clients see this as the top-level folder (S3: the bucket name). No slashes.</span>
    </label>
    <div class="flex items-end gap-4 pb-4">
     <label class="text-xs text-slate-600 dark:text-slate-300 inline-flex items-center gap-2 cursor-pointer"><input type="checkbox" bind:checked={draft.read_only} /> Read-only</label>
     <label class="text-xs text-slate-600 dark:text-slate-300 inline-flex items-center gap-2 cursor-pointer" title="Mount at / instead of /<name>/"><input type="checkbox" bind:checked={draft.root} /> Root mount</label>
    </div>
   </div>

   <div class="mt-1">
    <div class="text-[11px] text-slate-500 dark:text-slate-400 mb-1">Mount paths — <code class="font-mono">&lt;mount-prefix&gt;</code> or <code class="font-mono">&lt;mount-prefix&gt;/&lt;sub/path&gt;</code></div>
    <div class="flex flex-col gap-1.5">
     {#each draft.paths as _, pi (pi)}
      <div class="flex items-center gap-2">
       <input type="text" class={inputCls + ' font-mono grow'} bind:value={draft.paths[pi]} list="serve-mount-prefixes" placeholder="data/releases" />
       <button
        type="button"
        class="px-1.5 py-1 rounded text-vermilion-600 dark:text-vermilion-300 hover:bg-vermilion-50 dark:hover:bg-vermilion-950/40 cursor-pointer disabled:opacity-40"
        title="Remove path"
        disabled={draft.paths.length <= 1}
        onclick={() => removePath(pi)}
       >
        <Trash2 size={12} />
       </button>
      </div>
     {/each}
     <button type="button" class="self-start text-xs text-accent-600 dark:text-accent-400 hover:text-accent-700 dark:hover:text-accent-300 inline-flex items-center gap-1 cursor-pointer" onclick={addPath}>
      <Plus size={11} /> Add path
     </button>
    </div>
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
     disabled={savingDraft || serveStore.saving || !draftValid}
    >{savingDraft ? 'Saving…' : 'Save share'}</button>
   </div>
  </div>
 {/if}

 <datalist id="serve-mount-prefixes">
  {#each mountPrefixes as pfx (pfx)}<option value={pfx}></option>{/each}
 </datalist>
</section>
