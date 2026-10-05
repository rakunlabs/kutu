<!--
 StringListInput — a text field that adds entries to a list of strings,
 shown as removable chips. Used for scopes, claim paths, superadmins,
 CIDRs and origins in the Authentication settings.
-->
<script lang="ts">
 import { Plus, X } from 'lucide-svelte';

 type Props = {
  values: string[];
  onchange: (next: string[]) => void;
  id: string;
  placeholder?: string;
  addLabel?: string;
  /** Accessible noun for remove buttons, e.g. "scope". */
  noun: string;
  mono?: boolean;
 };

 let { values, onchange, id, placeholder = '', addLabel = 'Add', noun, mono = true }: Props = $props();

 let draft = $state('');

 function add() {
  const v = draft.trim();
  if (!v || values.includes(v)) {
   draft = '';
   return;
  }
  onchange([...values, v]);
  draft = '';
 }

 function remove(i: number) {
  onchange(values.filter((_, idx) => idx !== i));
 }
</script>

<div class="flex gap-2">
 <input
  {id}
  type="text"
  class="input {mono ? 'font-mono' : ''}"
  bind:value={draft}
  {placeholder}
  onkeydown={(e) => {
   if (e.key === 'Enter') {
    e.preventDefault();
    add();
   }
  }}
 />
 <button type="button" class="btn btn-secondary shrink-0" onclick={add} disabled={!draft.trim()}><Plus size={13} /> {addLabel}</button>
</div>
{#if values.length > 0}
 <div class="mt-2 flex flex-wrap gap-1.5">
  {#each values as v, i (v)}
   <span class="chip {mono ? 'font-mono' : ''}">
    {v}
    <button type="button" class="x" onclick={() => remove(i)} aria-label="Remove {noun} {v}" title="Remove"><X size={11} /></button>
   </span>
  {/each}
 </div>
{/if}

<style>
 .chip {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
  height: 1.5rem;
  padding: 0 0.25rem 0 0.5rem;
  border: 1px solid var(--color-slate-300);
  border-radius: 2px;
  background: var(--color-slate-50);
  font-size: 12px;
  color: var(--color-slate-700);
 }

 .x {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 1.125rem;
  height: 1.125rem;
  border-radius: 2px;
  color: var(--color-slate-500);
  cursor: pointer;
 }

 .x:hover {
  color: var(--color-vermilion-600);
  background: var(--color-vermilion-50);
 }

 :global(.dark) .chip {
  border-color: var(--color-warm-600);
  background: var(--color-warm-900);
  color: var(--color-warm-200);
 }

 :global(.dark) .x {
  color: var(--color-warm-400);
 }

 :global(.dark) .x:hover {
  color: var(--color-vermilion-300);
  background: color-mix(in oklab, var(--color-vermilion-900) 60%, transparent);
 }
</style>
