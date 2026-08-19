<script lang="ts">
 // Serve users section — CRUD for the global user pool that FTP / SFTP /
 // WebDAV / S3 servers authenticate against (TFTP is anonymous). Users
 // are edited one at a time in an inline drawer with explicit Save /
 // Cancel; each save immediately PUTs the document, so there is no
 // page-wide "unsaved draft" limbo.
 //
 // A user needs a password or at least one SSH key; the Save button is
 // gated on that plus a unique, non-empty username, with the reason
 // shown inline instead of a surprise 400 from the backend.
 import { Users, Plus, Trash2, KeyRound, Eye, EyeOff } from 'lucide-svelte';
 import type { ServeUser } from '@/lib/types/config';
 import { serveStore } from '@/lib/store/serve.svelte';

 const users = $derived(serveStore.settings.users ?? []);
 const shareNames = $derived((serveStore.settings.shares ?? []).map(s => s.name).filter(Boolean));

 // editingIndex tracks which user row is open in the drawer. `null`
 // collapses it; -1 opens a fresh draft.
 let editingIndex = $state<number | null>(null);
 let draft = $state<ServeUser>(emptyDraft());
 let savingDraft = $state(false);
 let showPassword = $state(false);

 function emptyDraft(): ServeUser {
  return { username: '', password: '', shares: [], authorized_keys: '', read_only: false };
 }

 function openNew() {
  draft = emptyDraft();
  showPassword = false;
  editingIndex = -1;
 }

 function openEdit(i: number) {
  draft = structuredClone($state.snapshot(users[i])) as ServeUser;
  draft.shares ??= [];
  draft.password ??= '';
  draft.authorized_keys ??= '';
  showPassword = false;
  editingIndex = i;
 }

 function closeEditor() {
  editingIndex = null;
 }

 function toggleDraftShare(name: string, on: boolean) {
  const cur = new Set(draft.shares ?? []);
  if (on) cur.add(name); else cur.delete(name);
  draft.shares = [...cur];
 }

 // ── validation, surfaced inline next to the Save button ──
 const usernameTaken = $derived(
  users.some((u, i) => i !== editingIndex && u.username === draft.username.trim())
 );
 const draftProblem = $derived.by(() => {
  if (draft.username.trim() === '') return 'Username is required.';
  if (usernameTaken) return `Username "${draft.username.trim()}" is already taken.`;
  if (!draft.password && !(draft.authorized_keys ?? '').trim()) return 'Set a password or add an SSH key.';
  return null;
 });

 async function saveDraft() {
  savingDraft = true;
  try {
   const entry = structuredClone($state.snapshot(draft)) as ServeUser;
   entry.username = entry.username.trim();
   entry.authorized_keys = (entry.authorized_keys ?? '').trim();
   entry.shares = (entry.shares ?? []).filter(n => shareNames.includes(n));
   const next = users.map(u => structuredClone($state.snapshot(u)) as ServeUser);
   if (editingIndex === -1) next.push(entry);
   else next[editingIndex!] = entry;
   if (await serveStore.saveUsers(next)) closeEditor();
  } finally { savingDraft = false; }
 }

 async function deleteUser(i: number) {
  const u = users[i];
  if (!confirm(`Delete user "${u.username}"? Active sessions of this user will be dropped.`)) return;
  const next = users.filter((_, idx) => idx !== i).map(u => structuredClone($state.snapshot(u)) as ServeUser);
  await serveStore.saveUsers(next);
  if (editingIndex === i) closeEditor();
 }

 function sharesSummary(u: ServeUser): string {
  const n = (u.shares ?? []).length;
  return n === 0 ? 'all shares' : (n === 1 ? u.shares![0] : `${n} shares`);
 }

 const inputCls = 'rounded border border-slate-200 dark:border-warm-700 bg-white dark:bg-warm-900 px-2 py-1 text-xs';
 const labelCls = 'text-xs text-slate-600 dark:text-slate-300 flex flex-col gap-1';
</script>

<section class="mt-8">
 <div class="flex items-center justify-between mb-3">
  <h3 class="text-sm font-semibold text-slate-800 dark:text-slate-100 flex items-center gap-2">
   <Users size={15} class="text-accent-600 dark:text-accent-400" /> Users
  </h3>
  <button
   type="button"
   class="px-2.5 py-1 text-xs rounded bg-accent-600 text-white font-medium hover:bg-accent-700 inline-flex items-center gap-1.5 cursor-pointer"
   onclick={openNew}
  >
   <Plus size={12} /> New user
  </button>
 </div>

 {#if users.length === 0 && editingIndex !== -1}
  <p class="text-xs text-slate-400 dark:text-slate-500 border border-dashed border-slate-200 dark:border-warm-700 rounded-lg p-4 text-center">
   No users yet. FTP / SFTP / WebDAV / S3 servers need at least one user to accept connections.
   For S3, the username/password double as the access/secret key pair.
  </p>
 {:else}
  <div class="grid gap-3 md:grid-cols-2 lg:grid-cols-3">
   {#each users as u, i (u.username + '\u0000' + i)}
    <div class="bg-white dark:bg-warm-800 border border-slate-200 dark:border-warm-700 rounded-lg p-4 flex flex-col">
     <div class="flex items-start gap-2 mb-1">
      <Users size={15} class="text-accent-600 dark:text-accent-400 shrink-0 mt-0.5" />
      <div class="grow min-w-0">
       <button
        type="button"
        class="text-sm font-semibold text-slate-800 dark:text-slate-100 hover:text-accent-600 dark:hover:text-accent-400 truncate text-left cursor-pointer font-mono"
        onclick={() => openEdit(i)}
       >
        {u.username}
       </button>
       <div class="text-xs text-slate-500 dark:text-slate-400 truncate">
        {sharesSummary(u)}
       </div>
      </div>
     </div>

     <div class="flex flex-wrap items-center gap-1.5 mt-1 text-[10px]">
      {#if u.password}
       <span class="uppercase tracking-wide px-1.5 py-0.5 rounded bg-slate-100 text-slate-600 dark:bg-warm-900 dark:text-slate-400">password</span>
      {/if}
      {#if (u.authorized_keys ?? '').trim()}
       <span class="uppercase tracking-wide px-1.5 py-0.5 rounded bg-slate-100 text-slate-600 dark:bg-warm-900 dark:text-slate-400 inline-flex items-center gap-1"><KeyRound size={9} /> ssh key</span>
      {/if}
      {#if u.read_only}
       <span class="uppercase tracking-wide px-1.5 py-0.5 rounded bg-amber-100 text-amber-700 dark:bg-amber-950/40 dark:text-amber-300">read-only</span>
      {/if}
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
       title="Delete user"
       onclick={() => deleteUser(i)}
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
    {editingIndex === -1 ? 'New user' : `Edit "${users[editingIndex]?.username}"`}
   </h4>

   <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
    <label class={labelCls}>
     Username
     <input type="text" class={inputCls + ' font-mono'} bind:value={draft.username} autocomplete="off" placeholder="deploy" />
     <span class="text-[10px] text-slate-400">S3 clients use this as the access key.</span>
    </label>
    <label class={labelCls}>
     Password
     <span class="relative flex">
      {#if showPassword}
       <input type="text" class={inputCls + ' font-mono w-full pr-7'} bind:value={draft.password} autocomplete="off" placeholder="empty = key-only login" />
      {:else}
       <input type="password" class={inputCls + ' font-mono w-full pr-7'} bind:value={draft.password} autocomplete="new-password" placeholder="empty = key-only login" />
      {/if}
      <button
       type="button"
       class="absolute right-1.5 top-1/2 -translate-y-1/2 text-slate-400 hover:text-slate-600 dark:hover:text-slate-200 cursor-pointer"
       title={showPassword ? 'Hide password' : 'Show password'}
       onclick={() => (showPassword = !showPassword)}
      >
       {#if showPassword}<EyeOff size={13} />{:else}<Eye size={13} />{/if}
      </button>
     </span>
     <span class="text-[10px] text-slate-400">S3 clients use this as the secret key.</span>
    </label>
    <label class={labelCls + ' md:col-span-2'}>
     Authorized SSH keys (SFTP, optional — one per line)
     <textarea class={inputCls + ' font-mono'} rows="2" bind:value={draft.authorized_keys} placeholder="ssh-ed25519 AAAA..."></textarea>
    </label>
   </div>

   <div class="mt-3">
    <div class="text-[11px] text-slate-500 dark:text-slate-400 mb-1.5">
     Accessible shares <span class="text-slate-400">(none checked = all)</span>
    </div>
    {#if shareNames.length === 0}
     <p class="text-[11px] text-slate-400 italic">No shares defined yet — this user will see every share you add.</p>
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

   <div class="mt-4 flex items-center justify-between gap-3">
    <div class="flex items-center gap-4 min-w-0">
     <label class="text-xs text-slate-600 dark:text-slate-300 inline-flex items-center gap-2 cursor-pointer shrink-0">
      <input type="checkbox" bind:checked={draft.read_only} /> Read-only
     </label>
     {#if draftProblem}
      <span class="text-[11px] text-amber-600 dark:text-amber-400 truncate">{draftProblem}</span>
     {/if}
    </div>
    <div class="flex items-center gap-2 shrink-0">
     <button
      type="button"
      class="px-3 py-1.5 text-xs rounded bg-slate-100 dark:bg-warm-900 text-slate-700 dark:text-slate-300 hover:bg-slate-200 dark:hover:bg-warm-700 cursor-pointer"
      onclick={closeEditor}
     >Cancel</button>
     <button
      type="button"
      class="px-3 py-1.5 text-xs rounded bg-accent-600 text-white font-medium hover:bg-accent-700 disabled:opacity-50 disabled:cursor-not-allowed cursor-pointer"
      onclick={saveDraft}
      disabled={savingDraft || serveStore.saving || draftProblem !== null}
     >{savingDraft ? 'Saving…' : 'Save user'}</button>
    </div>
   </div>
  </div>
 {/if}
</section>
