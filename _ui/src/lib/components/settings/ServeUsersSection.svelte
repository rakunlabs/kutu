<script lang="ts">
 // Users — the global credential pool FTP / SFTP / WebDAV / S3 servers
 // authenticate against (TFTP is anonymous). Edited one at a time inline;
 // each save PUTs immediately.
 //
 // A user needs a password or at least one SSH key; Save is gated on
 // that plus a unique, non-empty username, with the reason shown inline.
 import { Plus, Trash2, KeyRound, Eye, EyeOff, Pencil } from 'lucide-svelte';
 import type { ServeUser } from '@/lib/types/config';
 import { serveStore } from '@/lib/store/serve.svelte';
 import { confirmAction } from '@/lib/store/confirm.svelte';
 import PanelHeader from './PanelHeader.svelte';
 import ChipPicker from './ChipPicker.svelte';

 const users = $derived(serveStore.settings.users ?? []);
 const shareNames = $derived((serveStore.settings.shares ?? []).map(s => s.name).filter(Boolean));

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
  if (editingIndex === i) { closeEditor(); return; }
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

 const usernameTaken = $derived(
  users.some((u, i) => i !== editingIndex && u.username === draft.username.trim())
 );
 const draftProblem = $derived.by(() => {
  if (draft.username.trim() === '') return 'Username is required.';
  if (usernameTaken) return `"${draft.username.trim()}" is already taken.`;
  if (!draft.password && !(draft.authorized_keys ?? '').trim()) return 'Set a password or add an SSH key.';
  return null;
 });

 async function saveDraft(e?: Event) {
  e?.preventDefault();
  if (draftProblem) return;
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
  const ok = await confirmAction({
   title: `Delete user "${u.username}"?`,
   message: 'Their open sessions are dropped and the credentials stop working on every server.',
   confirmLabel: 'Delete user',
   danger: true,
  });
  if (!ok) return;
  const next = users.filter((_, idx) => idx !== i).map(u => structuredClone($state.snapshot(u)) as ServeUser);
  await serveStore.saveUsers(next);
  if (editingIndex === i) closeEditor();
 }

 function sharesSummary(u: ServeUser): string {
  const n = (u.shares ?? []).length;
  return n === 0 ? 'All shares' : (n === 1 ? u.shares![0] : `${n} shares`);
 }
</script>

{#snippet editor()}
 <form class="p-4 pt-3" onsubmit={saveDraft}>
  <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
   <label class="field">
    <span class="field-label">Username</span>
    <input type="text" class="input font-mono" bind:value={draft.username} autocomplete="off" placeholder="deploy" />
    <span class="field-hint">S3 clients use this as the access key.</span>
   </label>
   <label class="field">
    <span class="field-label">Password</span>
    <span class="relative flex">
     {#if showPassword}
      <input type="text" class="input font-mono pr-9" bind:value={draft.password} autocomplete="off" placeholder="Empty for key-only login" />
     {:else}
      <input type="password" class="input font-mono pr-9" bind:value={draft.password} autocomplete="new-password" placeholder="Empty for key-only login" />
     {/if}
     <button type="button" class="btn btn-ghost btn-sm btn-icon absolute right-0.5 top-1/2 -translate-y-1/2" aria-label={showPassword ? 'Hide password' : 'Show password'} onclick={() => (showPassword = !showPassword)}>
      {#if showPassword}<EyeOff size={14} />{:else}<Eye size={14} />{/if}
     </button>
    </span>
    <span class="field-hint">S3 clients use this as the secret key.</span>
   </label>
   <label class="field md:col-span-2">
    <span class="field-label">Authorized SSH keys <span class="font-normal text-slate-500 dark:text-warm-400">SFTP, one per line</span></span>
    <textarea class="input font-mono" rows="2" bind:value={draft.authorized_keys} placeholder="ssh-ed25519 AAAA..."></textarea>
   </label>
  </div>

  <div class="mt-4 pt-4 border-t border-dashed border-slate-300 dark:border-warm-600">
   <ChipPicker
    label="Accessible shares"
    options={shareNames}
    bind:selected={draft.shares}
    emptyMeaning="Nothing selected grants every share."
    noOptions="No shares exist yet. This user will see every share you add."
   />
  </div>

  <div class="mt-5 flex flex-wrap items-center justify-end gap-3">
   <label class="check"><input type="checkbox" bind:checked={draft.read_only} /> Read-only</label>
   <span class="mr-auto text-[13px] {draftProblem && draft.username ? 'text-vermilion-700 dark:text-vermilion-300' : 'text-slate-500 dark:text-warm-400'}">{draftProblem && (draft.username || editingIndex !== -1) ? draftProblem : ''}</span>
   <button type="button" class="btn btn-secondary" onclick={closeEditor}>Cancel</button>
   <button type="submit" class="btn btn-primary" disabled={savingDraft || serveStore.saving || draftProblem !== null}>
    {savingDraft ? 'Saving…' : editingIndex === -1 ? 'Create user' : 'Save changes'}
   </button>
  </div>
 </form>
{/snippet}

<section>
 <PanelHeader title="Users" level={2}>
  Credentials for FTP, SFTP, WebDAV and S3. TFTP needs none.
  {#snippet actions()}
   <button type="button" class="btn btn-secondary btn-sm" onclick={openNew} disabled={editingIndex === -1}><Plus size={13} /> Add user</button>
  {/snippet}
 </PanelHeader>

 <div class="leaf overflow-hidden">
  {#if editingIndex === -1}
   <div class="border-b border-slate-200 dark:border-warm-700 bg-accent-50/50 dark:bg-accent-950/30">
    <div class="px-4 pt-4 text-[13px] font-semibold text-slate-900 dark:text-warm-50">New user</div>
    {@render editor()}
   </div>
  {/if}

  {#if users.length === 0 && editingIndex !== -1}
   <p class="px-6 py-8 text-center text-[13px] text-slate-600 dark:text-warm-300">No users yet. Servers other than TFTP refuse every login until you add one.</p>
  {:else}
   <ul class="divide-y divide-slate-200 dark:divide-warm-700">
    {#each users as u, i (u.username + '\u0000' + i)}
     {@const open = editingIndex === i}
     <li class={open ? 'bg-accent-50/50 dark:bg-accent-950/30' : ''}>
      <div class="flex flex-wrap md:flex-nowrap items-center gap-x-4 gap-y-1.5 px-4 py-3">
       <button type="button" class="font-mono text-[14px] font-semibold text-left text-slate-900 dark:text-warm-50 hover:text-accent-700 dark:hover:text-accent-300 cursor-pointer md:w-40 truncate" onclick={() => openEdit(i)} aria-expanded={open}>{u.username}</button>
       <span class="text-[13px] text-slate-600 dark:text-warm-300 flex-1 basis-32 min-w-0 truncate">{sharesSummary(u)}</span>
       <span class="flex items-center gap-1.5 md:w-48">
        {#if u.password}<span class="tag">password</span>{/if}
        {#if (u.authorized_keys ?? '').trim()}<span class="tag"><KeyRound size={10} /> ssh key</span>{/if}
        {#if u.read_only}<span class="tag">read-only</span>{/if}
       </span>
       <span class="flex items-center gap-1 ml-auto">
        <button type="button" class="btn btn-ghost btn-sm" onclick={() => openEdit(i)} aria-expanded={open} disabled={open}><Pencil size={13} /> Edit</button>
        <button type="button" class="btn btn-danger-ghost btn-sm btn-icon" onclick={() => deleteUser(i)} aria-label={`Delete user ${u.username}`} title="Delete user"><Trash2 size={14} /></button>
       </span>
      </div>
      {#if open}{@render editor()}{/if}
     </li>
    {/each}
   </ul>
  {/if}
 </div>
</section>
