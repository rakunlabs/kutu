<script lang="ts">
  // RegistrySearch — modal that searches package names across every
  // local and remote repository (virtuals are skipped server-side).
  import { Loader2, Search } from 'lucide-svelte';
  import Modal from '@/lib/components/Modal.svelte';
  import * as registryAPI from '@/lib/store/registry.svelte';
  import type { SearchHit } from './types';
  import { iconFor } from './utils';

  type Props = { open: boolean; onclose: () => void; onpick: (hit: SearchHit) => void };
  let { open, onclose, onpick }: Props = $props();

  let q = $state('');
  let hits = $state<SearchHit[]>([]);
  let loading = $state(false);
  let error = $state<string | null>(null);
  let timer: ReturnType<typeof setTimeout> | undefined;
  let seq = 0;

  function schedule() {
    clearTimeout(timer);
    timer = setTimeout(runSearch, 250);
  }

  async function runSearch() {
    const term = q.trim();
    const mine = ++seq;
    if (term.length < 2) {
      hits = [];
      error = null;
      return;
    }
    loading = true;
    try {
      const res = await registryAPI.searchPackages(term);
      if (mine === seq) {
        hits = res;
        error = null;
      }
    } catch (err) {
      if (mine === seq) error = String(err);
    } finally {
      if (mine === seq) loading = false;
    }
  }
</script>

<Modal {open} onClose={onclose} size="lg" ariaLabel="Search packages">
  {#snippet header()}
    <div class="flex items-center gap-2 w-full">
      <Search size={15} class="text-slate-500" />
      <!-- svelte-ignore a11y_autofocus -->
      <input
        class="flex-1 bg-transparent outline-none text-[15px]"
        placeholder="Search packages in every repository…"
        bind:value={q}
        oninput={schedule}
        autofocus
      />
      {#if loading}<Loader2 size={14} class="animate-spin text-slate-500" />{/if}
    </div>
  {/snippet}
  <div class="max-h-[60vh] overflow-y-auto">
    {#if error}
      <div class="p-4 text-[13px] text-vermilion-500">{error}</div>
    {:else if q.trim().length < 2}
      <div class="p-4 text-[13px] text-slate-500 dark:text-warm-400">Type at least two characters.</div>
    {:else if !loading && hits.length === 0}
      <div class="p-4 text-[13px] text-slate-500 dark:text-warm-400">No packages match “{q}”.</div>
    {:else}
      <ul>
        {#each hits as h (h.namespace + '/' + h.repo + '/' + h.name)}
          {@const Icon = iconFor(h.type)}
          <li class="border-b border-slate-200 dark:border-warm-700 last:border-b-0">
            <button class="w-full text-left px-4 py-2 hover:bg-slate-100 dark:hover:bg-warm-800 flex items-center gap-2" onclick={() => onpick(h)}>
              <Icon size={14} class="text-accent-500 shrink-0" />
              <span class="font-mono text-[13px] truncate">{h.name}</span>
              {#if h.latest}<span class="text-[11px] font-mono text-accent-500 shrink-0">@{h.latest}</span>{/if}
              <span class="ml-auto text-[11px] text-slate-500 dark:text-warm-400 font-mono shrink-0">{h.namespace}/{h.repo} · {h.type} · {h.kind}</span>
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </div>
</Modal>
