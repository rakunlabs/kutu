<script lang="ts">
 import { RefreshCw, HardDrive } from 'lucide-svelte';
 import RawFileTreeNode from './RawFileTreeNode.svelte';
 import { filesStore } from '@/lib/store/files.svelte';
 import { rawMountsStore } from '@/lib/store/rawmounts.svelte';
 import { onMount } from 'svelte';

 onMount(async () => {
 await rawMountsStore.load();
 filesStore.openFromURL(rawMountsStore.mounts);
 });

 async function refreshMounts() {
 await rawMountsStore.load();
 filesStore.initTree(rawMountsStore.mounts);
 }
</script>

<div class="flex flex-col h-full bg-slate-50 dark:bg-warm-950/60 border-r border-slate-200 dark:border-warm-700 select-none">
  <!-- Header -->
  <div class="flex items-center justify-between px-3 py-2 border-b border-slate-200 dark:border-warm-700">
  <span class="label-caps text-[11px] text-slate-500 dark:text-warm-400 flex items-center gap-1.5">
  <HardDrive size={12} class="text-accent-600 dark:text-accent-300" />
  Files
  </span>
  <div class="flex gap-0.5">
  <button
  class="btn btn-ghost btn-sm btn-icon !h-6 !w-6"
 onclick={refreshMounts}
 title="Refresh all"
 aria-label="Refresh all mounts"
 >
 <RefreshCw size={14} />
 </button>
 </div>
 </div>

 <!-- Tree -->
 <div class="flex-1 overflow-y-auto py-1" role="tree">
 {#if filesStore.tree.length > 0}
 {#each filesStore.tree as node (`${node.type}:${node.path}`)}
 <RawFileTreeNode {node} />
 {/each}
 {:else}
 <div class="p-5 text-center text-slate-500 dark:text-warm-400 text-[13px]">No mounts configured</div>
 {/if}
 </div>
</div>
