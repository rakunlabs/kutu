<script lang="ts">
 import { untrack } from 'svelte';
 import { X } from 'lucide-svelte';
 import Modal from '@/lib/components/Modal.svelte';
 import { apiServerMessage } from '@/lib/api/client';
 import { appStore, type PermissionInfo } from '@/lib/store/store.svelte';
 import { addToast } from '@/lib/store/toast.svelte';
 import PermissionPatternEditor from './PermissionPatternEditor.svelte';
 import { cleanPatterns, patternsEqual, type KnownCapability } from './userQuery.svelte';

 type Props = {
  perm: PermissionInfo;
  knownKeys: KnownCapability[];
  onClose: () => void;
 };

 let { perm, knownKeys, onClose }: Props = $props();

 // Snapshot the permission on mount; edits never touch the cached copy.
 const initial = untrack(() => perm);
 let editKey = $state(initial.key);
 let editName = $state(initial.name);
 let editDesc = $state(initial.description);
 let editKeys = $state<string[]>([...(initial.keys || [])]);
 let editPatterns = $state<Record<string, string[]>>(
  Object.fromEntries(Object.entries(initial.key_patterns ?? {}).map(([k, v]) => [k, [...v]])),
 );
 let saving = $state(false);

 async function save() {
  saving = true;
  try {
   const updates: {
    key?: string;
    name?: string;
    description?: string;
    keys?: string[];
    key_patterns?: Record<string, string[]>;
   } = {};
   if (editKey !== perm.key) updates.key = editKey;
   if (editName !== perm.name) updates.name = editName;
   if (editDesc !== perm.description) updates.description = editDesc;
   const origKeys = [...(perm.keys || [])].sort().join(',');
   const newKeys = [...editKeys].sort().join(',');
   if (origKeys !== newKeys) updates.keys = editKeys;

   // `key_patterns: {}` tells the backend to clear all patterns.
   const cleaned = cleanPatterns(editPatterns, editKeys);
   if (!patternsEqual(cleaned, perm.key_patterns)) {
    updates.key_patterns = cleaned ?? {};
   }

   if (Object.keys(updates).length > 0) {
    await appStore.updatePermission(perm.id, updates);
    addToast('Permission updated', 'success');
   }
   onClose();
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not update the permission'), 'alert');
  } finally {
   saving = false;
  }
 }
</script>

<Modal open={true} {onClose} size="md" ariaLabel="Edit permission {perm.name}">
 {#snippet header()}
  <h2 class="text-[15px] font-semibold text-slate-900 dark:text-warm-50 truncate">Edit permission <span class="font-mono">{perm.key}</span></h2>
  <button type="button" class="btn btn-ghost btn-sm btn-icon" onclick={onClose} aria-label="Close"><X size={15} /></button>
 {/snippet}

 <div class="p-4 space-y-4">
  <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
   <label class="field">
    <span class="field-label">Name</span>
    <input type="text" class="input" bind:value={editName} />
   </label>
   <label class="field">
    <span class="field-label">Slug</span>
    <input type="text" class="input font-mono" bind:value={editKey} />
   </label>
   <label class="field sm:col-span-2">
    <span class="field-label">Description</span>
    <input type="text" class="input" bind:value={editDesc} />
   </label>
  </div>
  <PermissionPatternEditor {knownKeys} bind:keys={editKeys} bind:patterns={editPatterns} />
 </div>

 {#snippet footer()}
  <button type="button" class="btn btn-secondary" onclick={onClose}>Cancel</button>
  <button type="button" class="btn btn-primary" onclick={save} disabled={saving || !editName || editKeys.length === 0}>
   {saving ? 'Saving…' : 'Save changes'}
  </button>
 {/snippet}
</Modal>
