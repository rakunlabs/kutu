<script lang="ts">
 // /settings — deployment configuration. A left sidebar switches
 // between sections; the main pane renders the active one:
 //
 //   - Raw mounts : CRUD for the storage backends every other feature
 //                  reads from (file browser, registries, proxy).
 //   - About      : build metadata (version / commit / date) and a link
 //                  to the source repository.
 //
 // Coloring follows pika's DESIGN_SYSTEM.md (the "Active nav-item"
 // soft-selection pattern + the About dl card), so this page matches
 // the rest of the rakunlabs UI family.
 import { onMount } from 'svelte';
 import { HardDrive, Info, ExternalLink, Copy, Check } from 'lucide-svelte';
 import { rawMountsStore } from '@/lib/store/rawmounts.svelte';
 import { appStore } from '@/lib/store/store.svelte';
 import { addToast } from '@/lib/store/toast.svelte';
 import RawMountsPanel from '@/lib/components/settings/RawMountsPanel.svelte';

 // Source repository — derived from the Go module path
 // (github.com/rakunlabs/kutu). Used to build commit/release links.
 const REPO_URL = 'https://github.com/rakunlabs/kutu';

 type Section = 'mounts' | 'about';
 const sections: { key: Section; label: string; icon: typeof HardDrive }[] = [
  { key: 'mounts', label: 'Raw mounts', icon: HardDrive },
  { key: 'about', label: 'About', icon: Info },
 ];

 let activeSection = $state<Section>('mounts');

 const info = $derived(appStore.info);

 // ldflags use a placeholder of "-" when the build was not stamped
 // (e.g. `go run` without -ldflags). Treat that as "unknown".
 const hasCommit = $derived(!!info?.commit && info.commit !== '-');
 const hasDate = $derived(!!info?.date && info.date !== '-');
 const hasVersion = $derived(!!info?.version && info.version !== 'v0.0.0');

 const commitUrl = $derived(hasCommit ? `${REPO_URL}/commit/${info?.commit}` : '');
 const versionUrl = $derived(hasVersion ? `${REPO_URL}/releases/tag/${info?.version}` : '');

 // Build date is stamped "YYYY-MM-DD_HH:MM:SS" (UTC). Render friendlier
 // without localizing — it's a build artifact, not a user timestamp.
 const formattedDate = $derived.by(() => {
  if (!hasDate) return '';
  return (info?.date ?? '').replace('_', ' ') + ' UTC';
 });

 let copied = $state<string | null>(null);

 async function copyToClipboard(value: string, label: string) {
  try {
   await navigator.clipboard.writeText(value);
   copied = label;
   addToast(`${label} copied to clipboard`, 'success');
   setTimeout(() => { if (copied === label) copied = null; }, 2000);
  } catch {
   addToast('Failed to copy to clipboard', 'alert');
  }
 }

 onMount(() => {
  void rawMountsStore.load();
 });
</script>

<svelte:head><title>Settings · kutu</title></svelte:head>

<div class="flex h-full overflow-hidden bg-slate-100 dark:bg-warm-900">
 <!-- Left Sidebar -->
 <div class="w-52 shrink-0 bg-slate-50 dark:bg-warm-800 border-r border-slate-200 dark:border-warm-700 overflow-y-auto">
  <nav class="flex flex-col gap-0.5 px-2 pt-3 pb-4">
   {#each sections as section (section.key)}
    <button
     class="flex items-center gap-2.5 w-full px-3 py-2 text-[13px] font-medium rounded-md cursor-pointer transition-colors text-left
{activeSection === section.key
      ? 'bg-accent-50 text-accent-700 border border-accent-200 dark:bg-accent-900/40 dark:text-accent-300 dark:border-accent-700'
      : 'bg-transparent text-slate-600 dark:text-warm-200 border border-transparent hover:bg-slate-100 dark:hover:bg-warm-700 hover:text-slate-800 dark:hover:text-white'}"
     onclick={() => (activeSection = section.key)}
    >
     <section.icon size={15} class="shrink-0" />
     {section.label}
    </button>
   {/each}
  </nav>
 </div>

 <!-- Right Content Area -->
 <div class="flex-1 overflow-y-auto">
  {#if activeSection === 'mounts'}
   <div class="max-w-6xl p-6">
    {#if !rawMountsStore.loaded}
     <div class="text-sm text-slate-500 dark:text-slate-400 py-10 text-center">Loading…</div>
    {:else}
     <RawMountsPanel configs={rawMountsStore.configs} mounts={rawMountsStore.mounts} />
    {/if}
   </div>
  {:else if activeSection === 'about'}
   <div class="max-w-3xl p-6">
    <div class="mb-4">
     <h2 class="text-lg font-semibold text-slate-800 dark:text-slate-100">About</h2>
     <p class="text-sm text-slate-500 dark:text-slate-400 mt-0.5">
      Build and source information for this kutu instance
     </p>
    </div>

    <div class="p-5 bg-white dark:bg-warm-900 border border-slate-200 dark:border-warm-700 rounded-lg shadow-sm">
     <dl class="divide-y divide-slate-100 dark:divide-warm-800">
      <!-- Name -->
      <div class="flex items-baseline gap-4 py-2.5 first:pt-0">
       <dt class="w-32 shrink-0 text-xs font-medium text-slate-500 dark:text-slate-400 uppercase tracking-wider">Name</dt>
       <dd class="flex-1 text-sm text-slate-800 dark:text-slate-100">{info?.name ?? 'kutu'}</dd>
      </div>

      <!-- Version -->
      <div class="flex items-baseline gap-4 py-2.5">
       <dt class="w-32 shrink-0 text-xs font-medium text-slate-500 dark:text-slate-400 uppercase tracking-wider">Version</dt>
       <dd class="flex-1 text-sm text-slate-800 dark:text-slate-100 flex items-center gap-2">
        <span class="font-mono">{info?.version ?? 'unknown'}</span>
        {#if hasVersion}
         <a
          href={versionUrl}
          target="_blank"
          rel="noopener noreferrer"
          class="inline-flex items-center gap-1 text-xs text-accent-600 hover:text-accent-700 dark:text-accent-300 dark:hover:text-accent-200 cursor-pointer"
          title="View release on GitHub"
         >
          <ExternalLink size={12} />
          release
         </a>
        {/if}
       </dd>
      </div>

      <!-- Commit -->
      <div class="flex items-baseline gap-4 py-2.5">
       <dt class="w-32 shrink-0 text-xs font-medium text-slate-500 dark:text-slate-400 uppercase tracking-wider">Commit</dt>
       <dd class="flex-1 text-sm text-slate-800 dark:text-slate-100 flex items-center gap-2">
        {#if hasCommit}
         <a
          href={commitUrl}
          target="_blank"
          rel="noopener noreferrer"
          class="font-mono text-accent-600 hover:text-accent-700 dark:text-accent-300 dark:hover:text-accent-200 hover:underline cursor-pointer"
          title="View commit on GitHub"
         >
          {info?.commit}
         </a>
         <button
          type="button"
          class="inline-flex items-center text-slate-400 dark:text-slate-500 hover:text-slate-700 dark:hover:text-slate-200 cursor-pointer"
          onclick={() => copyToClipboard(info?.commit ?? '', 'Commit')}
          title="Copy commit hash"
          aria-label="Copy commit hash"
         >
          {#if copied === 'Commit'}
           <Check size={12} class="text-green-600" />
          {:else}
           <Copy size={12} />
          {/if}
         </button>
        {:else}
         <span class="text-slate-400 dark:text-slate-500 italic">unknown</span>
        {/if}
       </dd>
      </div>

      <!-- Build date -->
      <div class="flex items-baseline gap-4 py-2.5">
       <dt class="w-32 shrink-0 text-xs font-medium text-slate-500 dark:text-slate-400 uppercase tracking-wider">Build Date</dt>
       <dd class="flex-1 text-sm text-slate-800 dark:text-slate-100">
        {#if hasDate}
         <span class="font-mono">{formattedDate}</span>
        {:else}
         <span class="text-slate-400 dark:text-slate-500 italic">unknown</span>
        {/if}
       </dd>
      </div>

      <!-- Repository -->
      <div class="flex items-baseline gap-4 py-2.5">
       <dt class="w-32 shrink-0 text-xs font-medium text-slate-500 dark:text-slate-400 uppercase tracking-wider">Repository</dt>
       <dd class="flex-1 text-sm text-slate-800 dark:text-slate-100 flex items-center gap-2">
        <a
         href={REPO_URL}
         target="_blank"
         rel="noopener noreferrer"
         class="text-accent-600 hover:text-accent-700 dark:text-accent-300 dark:hover:text-accent-200 hover:underline cursor-pointer break-all"
        >
         {REPO_URL}
        </a>
        <a
         href={REPO_URL}
         target="_blank"
         rel="noopener noreferrer"
         class="inline-flex items-center text-slate-400 dark:text-slate-500 hover:text-slate-700 dark:hover:text-slate-200 cursor-pointer"
         title="Open in new tab"
         aria-label="Open repository in new tab"
        >
         <ExternalLink size={12} />
        </a>
       </dd>
      </div>
     </dl>
    </div>
   </div>
  {/if}
 </div>
</div>
