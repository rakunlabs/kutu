<script lang="ts">
 // Shares — CRUD for the global share pool. A share maps a name to one
 // or more raw-mount paths ("<mount-prefix>" or "<mount-prefix>/<sub>");
 // servers then expose all shares or a named subset.
 //
 // Renaming or deleting a share also prunes stale references from
 // servers and users in the same PUT so backend validation never trips
 // over a dangling name.
 import { Plus, Trash2, Pencil, X } from 'lucide-svelte';
 import type { ServeShare, ServeServerEntry, ServeUser } from '@/lib/types/config';
 import { serveStore } from '@/lib/store/serve.svelte';
 import { rawMountsStore } from '@/lib/store/rawmounts.svelte';
 import { confirmAction } from '@/lib/store/confirm.svelte';
 import PanelHeader from './PanelHeader.svelte';

 const shares = $derived(serveStore.settings.shares ?? []);
 const mountPrefixes = $derived(rawMountsStore.configs.map(c => c.prefix));

 // Which servers expose a given share (explicitly or via "all").
 function usedBy(name: string): string {
  const servers = serveStore.settings.servers ?? [];
  const explicit = servers.filter(s => (s.shares ?? []).includes(name));
  const implicit = servers.filter(s => (s.shares ?? []).length === 0);
  const n = explicit.length + implicit.length;
  if (servers.length === 0 || n === 0) return 'Not served';
  if (n === servers.length) return 'All servers';
  return `${n} server${n === 1 ? '' : 's'}`;
 }

 // editingIndex: `null` collapsed, -1 new draft, otherwise the row.
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
  if (editingIndex === i) { closeEditor(); return; }
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

 const draftProblem = $derived.by(() => {
  const n = draft.name.trim();
  if (!n) return 'Name is required.';
  if (/[/\\]/.test(n)) return 'Names cannot contain slashes.';
  if (shares.some((s, i) => i !== editingIndex && s.name === n)) return `"${n}" already exists.`;
  if (!draft.paths.some(p => p.trim() !== '')) return 'Add at least one mount path.';
  return null;
 });

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

 async function saveDraft(e?: Event) {
  e?.preventDefault();
  if (draftProblem) return;
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
  const ok = await confirmAction({
   title: `Delete share "${sh.name}"?`,
   message: 'Servers and users that list it lose access. The files in the raw mount stay where they are.',
   confirmLabel: 'Delete share',
   danger: true,
  });
  if (!ok) return;
  const next = shares.filter((_, idx) => idx !== i).map(s => structuredClone($state.snapshot(s)) as ServeShare);
  await fullDocSave(next);
  if (editingIndex === i) closeEditor();
 }
</script>

{#snippet editor()}
 <form class="p-4 pt-3" onsubmit={saveDraft}>
  <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
   <label class="field">
    <span class="field-label">Name</span>
    <input type="text" class="input font-mono" bind:value={draft.name} placeholder="releases" />
    <span class="field-hint">The top-level folder clients see. Over S3 it is the bucket name.</span>
   </label>
   <div class="flex flex-col gap-2.5 md:pt-6">
    <label class="check"><input type="checkbox" bind:checked={draft.read_only} /> Read-only</label>
    <label class="check"><input type="checkbox" bind:checked={draft.root} /> Mount at <code class="font-mono text-[12px]">/</code> instead of <code class="font-mono text-[12px]">/{draft.name.trim() || 'name'}/</code></label>
   </div>
  </div>

  <fieldset class="mt-4 pt-4 border-t border-dashed border-slate-300 dark:border-warm-600">
   <legend class="field-label">Mount paths</legend>
   <p class="field-hint mt-1"><code class="font-mono">&lt;mount-prefix&gt;</code> or <code class="font-mono">&lt;mount-prefix&gt;/&lt;sub/path&gt;</code>. Several paths merge into one share.</p>
   <div class="mt-2 flex flex-col gap-2">
    {#each draft.paths as _, pi (pi)}
     <div class="flex items-center gap-2">
      <input type="text" class="input font-mono" bind:value={draft.paths[pi]} list="serve-mount-prefixes" placeholder="data/releases" aria-label={`Mount path ${pi + 1}`} />
      <button type="button" class="btn btn-ghost btn-icon shrink-0" disabled={draft.paths.length <= 1} onclick={() => removePath(pi)} aria-label={`Remove mount path ${pi + 1}`} title="Remove path"><X size={14} /></button>
     </div>
    {/each}
    <button type="button" class="btn btn-ghost btn-sm self-start" onclick={addPath}><Plus size={13} /> Add path</button>
   </div>
  </fieldset>

  <div class="mt-5 flex flex-wrap items-center justify-end gap-3">
   {#if draftProblem && draft.name}
    <span class="mr-auto text-[13px] text-vermilion-700 dark:text-vermilion-300">{draftProblem}</span>
   {/if}
   <button type="button" class="btn btn-secondary" onclick={closeEditor}>Cancel</button>
   <button type="submit" class="btn btn-primary" disabled={savingDraft || serveStore.saving || !!draftProblem}>
    {savingDraft ? 'Saving…' : editingIndex === -1 ? 'Create share' : 'Save changes'}
   </button>
  </div>
 </form>
{/snippet}

<section>
 <PanelHeader title="Shares" level={2}>
  Named folders built from raw-mount paths.
  {#snippet actions()}
   <button type="button" class="btn btn-secondary btn-sm" onclick={openNew} disabled={editingIndex === -1}><Plus size={13} /> Add share</button>
  {/snippet}
 </PanelHeader>

 <div class="leaf overflow-hidden">
  {#if editingIndex === -1}
   <div class="border-b border-slate-200 dark:border-warm-700 bg-accent-50/50 dark:bg-accent-950/30">
    <div class="px-4 pt-4 text-[13px] font-semibold text-slate-900 dark:text-warm-50">New share</div>
    {@render editor()}
   </div>
  {/if}

  {#if shares.length === 0 && editingIndex !== -1}
   <p class="px-6 py-8 text-center text-[13px] text-slate-600 dark:text-warm-300">No shares yet. Servers have nothing to serve until you add one.</p>
  {:else}
   <ul class="divide-y divide-slate-200 dark:divide-warm-700">
    {#each shares as sh, i (sh.name + '\u0000' + i)}
     {@const open = editingIndex === i}
     <li class={open ? 'bg-accent-50/50 dark:bg-accent-950/30' : ''}>
      <div class="flex flex-wrap md:flex-nowrap items-center gap-x-4 gap-y-1.5 px-4 py-3">
       <button type="button" class="font-mono text-[14px] font-semibold text-left text-slate-900 dark:text-warm-50 hover:text-accent-700 dark:hover:text-accent-300 cursor-pointer md:w-40 truncate" onclick={() => openEdit(i)} aria-expanded={open}>{sh.name}</button>
       <span class="font-mono text-[13px] text-slate-600 dark:text-warm-300 truncate flex-1 basis-40 min-w-0" title={sh.paths.join(', ')}>{sh.paths.join(', ') || '—'}</span>
       <span class="flex items-center gap-1.5 md:w-48">
        <span class="text-[13px] text-slate-600 dark:text-warm-300">{usedBy(sh.name)}</span>
        {#if sh.read_only}<span class="tag">read-only</span>{/if}
        {#if sh.root}<span class="tag">root</span>{/if}
       </span>
       <span class="flex items-center gap-1 ml-auto">
        <button type="button" class="btn btn-ghost btn-sm" onclick={() => openEdit(i)} aria-expanded={open} disabled={open}><Pencil size={13} /> Edit</button>
        <button type="button" class="btn btn-danger-ghost btn-sm btn-icon" onclick={() => deleteShare(i)} aria-label={`Delete share ${sh.name}`} title="Delete share"><Trash2 size={14} /></button>
       </span>
      </div>
      {#if open}{@render editor()}{/if}
     </li>
    {/each}
   </ul>
  {/if}
 </div>

 <datalist id="serve-mount-prefixes">
  {#each mountPrefixes as pfx (pfx)}<option value={pfx}></option>{/each}
 </datalist>
</section>
