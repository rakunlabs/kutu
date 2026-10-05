<script lang="ts">
 import { Pencil, Plus, Trash2, Users as UsersIcon } from 'lucide-svelte';
 import { apiServerMessage } from '@/lib/api/client';
 import { appStore, type PermissionInfo } from '@/lib/store/store.svelte';
 import { addToast } from '@/lib/store/toast.svelte';
 import { confirmAction } from '@/lib/store/confirm.svelte';
 import PanelHeader from '@/lib/components/settings/PanelHeader.svelte';
 import PermissionPatternEditor from './PermissionPatternEditor.svelte';
 import { cleanPatterns, type KnownCapability } from './userQuery.svelte';

 type Props = {
  knownKeys: KnownCapability[];
  canManageUsers: boolean;
  onEdit: (perm: PermissionInfo) => void;
  onViewUsers: (permissionId: string) => void;
 };

 let { knownKeys, canManageUsers, onEdit, onViewUsers }: Props = $props();

 const allPermissions = $derived(appStore.permissions);

 let showCreate = $state(false);
 let newKey = $state('');
 let newName = $state('');
 let newDesc = $state('');
 let newKeys = $state<string[]>([]);
 let newPatterns = $state<Record<string, string[]>>({});
 let creating = $state(false);

 function resetForm() {
  newKey = '';
  newName = '';
  newDesc = '';
  newKeys = [];
  newPatterns = {};
 }

 async function handleCreate(e?: Event) {
  e?.preventDefault();
  if (!newName || newKeys.length === 0) return;
  creating = true;
  const key = newKey || newName.toLowerCase().replace(/\s+/g, '-').replace(/[^a-z0-9.-]/g, '');
  try {
   await appStore.createPermission(key, newName, newDesc, newKeys, cleanPatterns(newPatterns, newKeys));
   addToast(`Permission "${newName}" created`, 'success');
   resetForm();
   showCreate = false;
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not create the permission'), 'alert');
  } finally {
   creating = false;
  }
 }

 async function handleDelete(perm: PermissionInfo) {
  const ok = await confirmAction({
   title: `Delete permission "${perm.name}"?`,
   message: 'Users holding it lose these capabilities on their next request. The users themselves are kept.',
   confirmLabel: 'Delete permission',
   danger: true,
  });
  if (!ok) return;
  try {
   await appStore.deletePermission(perm.id);
   addToast('Permission deleted', 'success');
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not delete the permission'), 'alert');
  }
 }
</script>

<section>
 <PanelHeader title="Permissions" level={2}>
  Named bundles of capabilities, optionally limited to certain mounts or repositories.
  {#snippet actions()}
   <button type="button" class="btn btn-secondary btn-sm" onclick={() => (showCreate = true)} disabled={showCreate}><Plus size={13} /> Add permission</button>
  {/snippet}
 </PanelHeader>

 <div class="leaf overflow-hidden">
  {#if showCreate}
   <form class="p-4 border-b border-slate-200 dark:border-warm-700 bg-accent-50/50 dark:bg-accent-950/30" onsubmit={handleCreate}>
    <div class="text-[13px] font-semibold text-slate-900 dark:text-warm-50 mb-3">New permission</div>
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4 mb-4">
     <label class="field">
      <span class="field-label">Name</span>
      <input type="text" class="input" bind:value={newName} placeholder="Team A publishers" />
     </label>
     <label class="field">
      <span class="field-label">Slug <span class="font-normal text-slate-500 dark:text-warm-400">generated from the name if empty</span></span>
      <input type="text" class="input font-mono" bind:value={newKey} placeholder="team-a-publish" />
     </label>
     <label class="field md:col-span-2">
      <span class="field-label">Description</span>
      <input type="text" class="input" bind:value={newDesc} placeholder="What this permission grants" />
     </label>
    </div>
    <PermissionPatternEditor {knownKeys} bind:keys={newKeys} bind:patterns={newPatterns} />
    <div class="mt-5 flex items-center justify-end gap-2">
     <button type="button" class="btn btn-secondary" onclick={() => { showCreate = false; resetForm(); }}>Cancel</button>
     <button type="submit" class="btn btn-primary" disabled={creating || !newName || newKeys.length === 0}>
      {creating ? 'Creating…' : 'Create permission'}
     </button>
    </div>
   </form>
  {/if}

  {#if allPermissions.length === 0}
   {#if !showCreate}
    <p class="px-6 py-8 text-center text-[13px] text-slate-600 dark:text-warm-300">No permissions yet. Create one, then assign it to users.</p>
   {/if}
  {:else}
   <ul class="divide-y divide-slate-200 dark:divide-warm-700">
    {#each allPermissions as perm (perm.id)}
     <li class="flex flex-wrap md:flex-nowrap items-start gap-x-4 gap-y-2 px-4 py-3">
      <div class="min-w-0 md:w-56 shrink-0">
       <button type="button" class="text-[14px] font-semibold text-left text-slate-900 dark:text-warm-50 hover:text-accent-700 dark:hover:text-accent-300 cursor-pointer truncate max-w-full" onclick={() => onEdit(perm)}>{perm.name}</button>
       <div class="font-mono text-[12px] text-slate-500 dark:text-warm-400 truncate">{perm.key}</div>
       {#if perm.description}<div class="text-[12px] text-slate-600 dark:text-warm-300 mt-0.5">{perm.description}</div>{/if}
      </div>
      <div class="flex flex-wrap gap-1 flex-1 min-w-0">
       {#each perm.keys || [] as k}
        {@const pats = perm.key_patterns?.[k] ?? []}
        <span class="tag !normal-case !tracking-normal font-mono" title={pats.length ? pats.join('\n') : undefined}>
         {k}{#if pats.length > 0}<span class="text-accent-700 dark:text-accent-300">· {pats.length} {pats.length === 1 ? 'path' : 'paths'}</span>{/if}
        </span>
       {:else}
        <span class="text-[12px] text-slate-500 dark:text-warm-400">No capabilities</span>
       {/each}
      </div>
      <span class="flex items-center gap-1 ml-auto shrink-0">
       {#if canManageUsers}
        <button type="button" class="btn btn-ghost btn-sm btn-icon" onclick={() => onViewUsers(perm.id)} title="Show users with this permission" aria-label="Show users with permission {perm.name}"><UsersIcon size={14} /></button>
       {/if}
       <button type="button" class="btn btn-ghost btn-sm" onclick={() => onEdit(perm)}><Pencil size={13} /> Edit</button>
       <button type="button" class="btn btn-danger-ghost btn-sm btn-icon" onclick={() => handleDelete(perm)} title="Delete permission" aria-label="Delete permission {perm.name}"><Trash2 size={14} /></button>
      </span>
     </li>
    {/each}
   </ul>
  {/if}
 </div>
</section>
