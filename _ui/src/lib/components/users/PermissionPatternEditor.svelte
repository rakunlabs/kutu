<!--
 PermissionPatternEditor — capability picker plus the optional per-
 capability path scoping. Shared by the "New permission" form and the
 edit modal.

 Pattern semantics in kutu: raw.* patterns match "<mount>/<path>",
 registry.* patterns match "<namespace>/<repo>/...". Other capabilities
 are global and ignore patterns.
-->
<script lang="ts">
 import { Check, Plus, Trash2 } from 'lucide-svelte';
 import type { KnownCapability } from './userQuery.svelte';

 type Props = {
  knownKeys: KnownCapability[];
  keys: string[];
  patterns: Record<string, string[]>;
 };

 let { knownKeys, keys = $bindable(), patterns = $bindable() }: Props = $props();

 let showHelp = $state(false);

 function isScopable(k: string): boolean {
  return k.startsWith('raw.') || k.startsWith('registry.');
 }

 function placeholderFor(k: string): string {
  return k.startsWith('raw.') ? 'builds/**' : 'team-a/**';
 }

 function toggleKey(key: string) {
  if (keys.includes(key)) {
   keys = keys.filter((k) => k !== key);
   if (patterns[key]) {
    const { [key]: _, ...rest } = patterns;
    patterns = rest;
   }
  } else {
   keys = [...keys, key];
  }
 }

 function addPattern(key: string) {
  patterns = { ...patterns, [key]: [...(patterns[key] ?? []), ''] };
 }

 function updatePattern(key: string, idx: number, value: string) {
  const arr = [...(patterns[key] ?? [])];
  arr[idx] = value;
  patterns = { ...patterns, [key]: arr };
 }

 function removePattern(key: string, idx: number) {
  const arr = (patterns[key] ?? []).filter((_, i) => i !== idx);
  if (arr.length === 0) {
   const { [key]: _drop, ...rest } = patterns;
   patterns = rest;
  } else {
   patterns = { ...patterns, [key]: arr };
  }
 }

 const scopedKeys = $derived(keys.filter(isScopable));
</script>

<fieldset>
 <legend class="field-label">Capabilities</legend>
 <p class="field-hint mt-1">{keys.length === 0 ? 'Pick at least one.' : `${keys.length} of ${knownKeys.length} selected.`}</p>
 <div class="mt-2 grid grid-cols-1 sm:grid-cols-2 gap-1.5">
  {#each knownKeys as known (known.key)}
   {@const selected = keys.includes(known.key)}
   <button type="button" onclick={() => toggleKey(known.key)} aria-pressed={selected} class="cap {selected ? 'is-on' : ''}">
    <span class="box" aria-hidden="true">{#if selected}<Check size={11} />{/if}</span>
    <span class="min-w-0">
     <span class="block font-mono text-[13px] font-semibold">{known.key}</span>
     {#if known.description}
      <span class="block text-[12px] leading-snug text-slate-500 dark:text-warm-400 mt-0.5">{known.description}</span>
     {/if}
    </span>
   </button>
  {/each}
 </div>
</fieldset>

{#if scopedKeys.length > 0}
 <div class="mt-4 pt-4 border-t border-dashed border-slate-300 dark:border-warm-600">
  <div class="flex items-baseline justify-between gap-3">
   <span class="field-label">Path scoping <span class="font-normal text-slate-500 dark:text-warm-400">optional</span></span>
   <button type="button" class="btn btn-ghost btn-sm" onclick={() => (showHelp = !showHelp)} aria-expanded={showHelp}>
    {showHelp ? 'Hide help' : 'How do patterns work?'}
   </button>
  </div>

  {#if showHelp}
   <div class="mt-2 p-3 rounded-[3px] border border-slate-300 dark:border-warm-600 bg-slate-50 dark:bg-warm-900 text-[13px] leading-relaxed text-slate-700 dark:text-warm-200 space-y-2 max-w-[70ch]">
    <p>
     <code class="font-mono">raw.*</code> patterns match <code class="font-mono">&lt;mount&gt;/&lt;path&gt;</code>, for example
     <code class="font-mono">builds/**</code> for everything in the <code class="font-mono">builds</code> mount.
     <code class="font-mono">registry.*</code> patterns match <code class="font-mono">&lt;namespace&gt;/&lt;repo&gt;/…</code>, for example
     <code class="font-mono">team-a/**</code>. No leading slash.
    </p>
    <p>
     <code class="font-mono">*</code> matches one segment, <code class="font-mono">**</code> any number of segments,
     <code class="font-mono">?</code> one character. Several patterns are OR'd.
    </p>
    <p class="text-slate-500 dark:text-warm-400">An empty list means unrestricted. Admin capabilities (users, tokens, settings) ignore patterns.</p>
   </div>
  {/if}

  <div class="mt-2 space-y-2">
   {#each scopedKeys as k (k)}
    {@const pats = patterns[k] ?? []}
    <div class="rounded-[3px] border border-slate-200 dark:border-warm-700 p-2.5">
     <div class="flex items-center justify-between gap-2">
      <code class="font-mono text-[13px] font-semibold">{k}</code>
      <button type="button" class="btn btn-ghost btn-sm" onclick={() => addPattern(k)}><Plus size={12} /> Pattern</button>
     </div>
     {#if pats.length === 0}
      <p class="field-hint mt-1">All paths.</p>
     {:else}
      <div class="mt-1.5 space-y-1.5">
       {#each pats as pat, i}
        <div class="flex gap-1.5 items-center">
         <input
          type="text"
          value={pat}
          oninput={(e) => updatePattern(k, i, e.currentTarget.value)}
          placeholder={placeholderFor(k)}
          aria-label="Path pattern for {k}"
          class="input font-mono !h-7"
         />
         <button type="button" class="btn btn-danger-ghost btn-sm btn-icon" onclick={() => removePattern(k, i)} title="Remove pattern" aria-label="Remove pattern"><Trash2 size={13} /></button>
        </div>
       {/each}
      </div>
     {/if}
    </div>
   {/each}
  </div>
 </div>
{/if}

<style>
 .cap {
  display: flex;
  align-items: flex-start;
  gap: 0.5rem;
  padding: 0.5rem 0.625rem;
  border: 1px solid var(--color-slate-300);
  border-radius: 3px;
  background: var(--color-white);
  text-align: left;
  cursor: pointer;
  color: var(--color-slate-700);
 }

 .cap:hover {
  border-color: var(--color-slate-500);
 }

 .cap.is-on {
  border-color: var(--sec-500);
  background: var(--sec-50);
  color: var(--sec-800);
 }

 .box {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 0.875rem;
  height: 0.875rem;
  margin-top: 0.1875rem;
  flex-shrink: 0;
  border-radius: 2px;
  border: 1px solid var(--color-slate-400);
  color: #fff;
 }

 .cap.is-on .box {
  background: var(--sec-600);
  border-color: var(--sec-600);
 }

 :global(.dark) .cap {
  background: var(--color-warm-950);
  border-color: var(--color-warm-600);
  color: var(--color-warm-200);
 }

 :global(.dark) .cap.is-on {
  border-color: var(--sec-400);
  background: color-mix(in oklab, var(--sec-900) 70%, transparent);
  color: var(--sec-100);
 }

 :global(.dark) .box {
  border-color: var(--color-warm-500);
 }
</style>
