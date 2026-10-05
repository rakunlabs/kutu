<script lang="ts">
 import type { Snippet } from 'svelte';

 let {
  title,
  level = 1,
  children,
  actions,
 }: {
  title: string;
  level?: 1 | 2;
  children?: Snippet;
  actions?: Snippet;
 } = $props();
</script>

<header class="flex flex-wrap items-end justify-between gap-x-6 gap-y-3 {level === 1 ? 'mb-6' : 'mb-3'}">
 <div class="min-w-0 max-w-[65ch]">
  {#if level === 1}
   <h1 class="text-[22px] font-bold tracking-tight text-slate-900 dark:text-warm-50">{title}</h1>
  {:else}
   <h2 class="label-caps text-[13px] text-slate-800 dark:text-warm-100">{title}</h2>
  {/if}
  {#if children}
   <p class="{level === 1 ? 'mt-1 text-[14px]' : 'mt-1 text-[13px]'} leading-relaxed text-slate-600 dark:text-warm-300">
    {@render children()}
   </p>
  {/if}
 </div>
 {#if actions}
  <div class="flex items-center gap-2 shrink-0">{@render actions()}</div>
 {/if}
</header>
