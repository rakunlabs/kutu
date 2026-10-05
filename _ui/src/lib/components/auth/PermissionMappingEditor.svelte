<!--
 PermissionMappingEditor — role → permissions / scope → permissions
 editor. Each row maps an external role (or OAuth2 scope) name to a set
 of kutu permission keys.
-->
<script lang="ts">
 import type { Snippet } from 'svelte';
 import { Plus, Trash2, X } from 'lucide-svelte';
 import type { PermissionInfo } from '@/lib/store/store.svelte';
 import type { MappingRow } from './types';

 type Props = {
  rows: MappingRow[];
  availablePermissions: PermissionInfo[];
  label: string;
  /** Noun used for buttons/placeholders, e.g. "role" or "scope". */
  noun: string;
  placeholder: string;
  emptyText?: string;
  inputId: string;
  description: Snippet;
 };

 let { rows = $bindable(), availablePermissions, label, noun, placeholder, emptyText, inputId, description }: Props = $props();

 let newKey = $state('');

 function addRow() {
  const k = newKey.trim();
  if (!k || rows.some((r) => r.key === k)) return;
  rows = [...rows, { key: k, permissions: [] }];
  newKey = '';
 }

 function removeRow(i: number) {
  rows = rows.filter((_, idx) => idx !== i);
 }

 function togglePerm(rowIdx: number, permKey: string) {
  rows = rows.map((r, i) => {
   if (i !== rowIdx) return r;
   const has = r.permissions.includes(permKey);
   return { key: r.key, permissions: has ? r.permissions.filter((c) => c !== permKey) : [...r.permissions, permKey] };
  });
 }

 // Keys that no longer match an existing permission (deleted or renamed).
 // Shown as removable chips so they aren't silently dropped on save.
 function unknownPermKeys(row: MappingRow): string[] {
  return row.permissions.filter((k) => !availablePermissions.some((p) => p.key === k));
 }
</script>

<div class="field">
 <label for={inputId} class="field-label">{label}</label>
 <p class="field-hint max-w-[70ch]">{@render description()}</p>
 <div class="flex gap-2 mt-1">
  <input
   id={inputId}
   type="text"
   class="input font-mono"
   bind:value={newKey}
   {placeholder}
   onkeydown={(e) => {
    if (e.key === 'Enter') {
     e.preventDefault();
     addRow();
    }
   }}
  />
  <button type="button" class="btn btn-secondary shrink-0" onclick={addRow} disabled={!newKey.trim()}><Plus size={13} /> Add {noun}</button>
 </div>
 {#if rows.length === 0}
  {#if emptyText}<p class="field-hint mt-1">{emptyText}</p>{/if}
 {:else}
  <div class="mt-2 space-y-2">
   {#each rows as row, rowIdx}
    <div class="rounded-[3px] border border-slate-200 dark:border-warm-700 p-3">
     <div class="flex items-center gap-2 mb-2">
      <input type="text" class="input font-mono !h-7" bind:value={row.key} placeholder="{noun} name" aria-label="{noun} name" />
      <button type="button" class="btn btn-danger-ghost btn-sm btn-icon" onclick={() => removeRow(rowIdx)} aria-label="Remove {noun} mapping" title="Remove mapping"><Trash2 size={13} /></button>
     </div>
     {#if availablePermissions.length === 0}
      <p class="field-hint">No permissions exist yet. Create them under Users &amp; permissions first.</p>
     {:else}
      <div class="grid grid-cols-1 sm:grid-cols-2 gap-x-4 gap-y-1.5">
       {#each availablePermissions as perm}
        <label class="check !items-start">
         <input type="checkbox" class="mt-0.5" checked={row.permissions.includes(perm.key)} onchange={() => togglePerm(rowIdx, perm.key)} />
         <span class="leading-tight">
          <span class="font-semibold">{perm.name}</span>
          <span class="block text-[12px] font-mono text-slate-500 dark:text-warm-400">{perm.key}</span>
         </span>
        </label>
       {/each}
      </div>
     {/if}
     {#if unknownPermKeys(row).length > 0}
      <div class="mt-2 flex flex-wrap items-center gap-1.5">
       <span class="text-[12px] text-slate-500 dark:text-warm-400">Unknown:</span>
       {#each unknownPermKeys(row) as uk}
        <button type="button" class="tag !normal-case font-mono cursor-pointer hover:!border-vermilion-500" onclick={() => togglePerm(rowIdx, uk)} title="Not an existing permission. Click to remove.">{uk} <X size={10} /></button>
       {/each}
      </div>
     {/if}
    </div>
   {/each}
  </div>
 {/if}
</div>
