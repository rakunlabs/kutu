<script lang="ts">
 // Multi-select as toggle chips. An empty selection has a meaning the
 // caller states (usually "all"), so it is spelled out next to the label.
 import { Check } from 'lucide-svelte';

 let {
  label,
  options,
  selected = $bindable([]),
  emptyMeaning,
  noOptions,
 }: {
  label: string;
  options: string[];
  selected?: string[];
  emptyMeaning: string;
  noOptions: string;
 } = $props();

 function toggle(name: string) {
  const cur = new Set(selected ?? []);
  if (cur.has(name)) cur.delete(name); else cur.add(name);
  selected = [...cur];
 }
</script>

<fieldset>
 <legend class="field-label">{label}</legend>
 {#if options.length === 0}
  <p class="field-hint mt-1.5">{noOptions}</p>
 {:else}
  <p class="field-hint mt-1">{(selected ?? []).length === 0 ? emptyMeaning : `${selected.length} of ${options.length} selected.`}</p>
  <div class="mt-2 flex flex-wrap gap-1.5">
   {#each options as name (name)}
    {@const on = (selected ?? []).includes(name)}
    <button type="button" class="chip font-mono {on ? 'is-on' : ''}" aria-pressed={on} onclick={() => toggle(name)}>
     {#if on}<Check size={12} />{/if}{name}
    </button>
   {/each}
  </div>
 {/if}
</fieldset>

<style>
 .chip {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
  height: 1.75rem;
  padding: 0 0.625rem;
  border-radius: 3px;
  border: 1px solid var(--color-slate-300);
  background: var(--color-white);
  font-size: 0.8125rem;
  color: var(--color-slate-700);
  cursor: pointer;
 }

 .chip:hover {
  border-color: var(--color-slate-500);
 }

 .chip.is-on {
  border-color: var(--sec-500);
  background: var(--sec-50);
  color: var(--sec-800);
 }

 :global(.dark) .chip {
  background: var(--color-warm-950);
  border-color: var(--color-warm-600);
  color: var(--color-warm-200);
 }

 :global(.dark) .chip.is-on {
  border-color: var(--sec-400);
  background: color-mix(in oklab, var(--sec-900) 70%, transparent);
  color: var(--sec-100);
 }
</style>
