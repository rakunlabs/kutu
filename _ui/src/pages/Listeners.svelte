<script lang="ts">
 // /listeners/:section — dedicated network endpoints. A left index (a
 // row of tabs on narrow screens) switches between listener kinds; the
 // section lives in the URL so reloads and shared links land in place.
 import { link, router } from 'svelte-spa-router';
 import { Boxes } from 'lucide-svelte';
 import { appStore } from '@/lib/store/store.svelte';
 import { addToast } from '@/lib/store/toast.svelte';
 import * as registryAPI from '@/lib/store/registry.svelte';
 import type { Namespace } from '@/lib/components/registry/types';
 import RegistryListenersPanel from '@/lib/components/listeners/RegistryListenersPanel.svelte';

 type Section = 'registry';
 type SectionDef = { key: Section; label: string; hint: string; icon: typeof Boxes; visible: () => boolean };

 const canRegistryAdmin = $derived(appStore.hasPermission('registry.admin'));
 const registryEnabled = $derived(appStore.info?.registry_enabled ?? true);

 const allSections: SectionDef[] = [
  { key: 'registry', label: 'Registry', hint: 'Repositories on own ports', icon: Boxes, visible: () => canRegistryAdmin && registryEnabled },
 ];
 const sections = $derived(allSections.filter(s => s.visible()));

 const activeSection = $derived.by<Section | null>(() => {
  const p = (router.params as Record<string, string> | null | undefined)?.section;
  return sections.some(s => s.key === p) ? (p as Section) : (sections[0]?.key ?? null);
 });
 const activeMeta = $derived(sections.find(s => s.key === activeSection));

 let namespaces = $state<Namespace[]>([]);
 let namespacesLoaded = $state(false);
 let namespacesRequested = false;

 $effect(() => {
  if (activeSection !== 'registry' || namespacesRequested) return;
  namespacesRequested = true;
  registryAPI.listRegistries()
   .then((body) => { namespaces = body.namespaces ?? []; })
   .catch((err) => addToast(`Failed to load registries: ${err}`, 'alert'))
   .finally(() => { namespacesLoaded = true; });
 });
</script>

<svelte:head><title>{activeMeta ? `${activeMeta.label} · ` : ''}Listeners · kutu</title></svelte:head>

<div class="flex flex-col md:flex-row h-full overflow-hidden">
 <nav
  aria-label="Listener sections"
  class="shrink-0 md:w-56 border-b md:border-b-0 md:border-r border-slate-300 dark:border-warm-700 bg-slate-50 dark:bg-warm-950/60 overflow-x-auto md:overflow-y-auto"
 >
  <div class="label-caps hidden md:block px-5 pt-5 pb-2 text-[12px] text-slate-500 dark:text-warm-400">Listeners</div>
  <ul class="flex md:flex-col gap-0.5 px-2 md:px-3 py-2 md:py-0 md:pb-4 min-w-max md:min-w-0">
   {#each sections as s (s.key)}
    {@const on = activeSection === s.key}
    <li>
     <a
      href={`/listeners/${s.key}`}
      use:link
      aria-current={on ? 'page' : undefined}
      class="group relative flex items-center gap-2.5 px-3 py-2 rounded-[3px] no-underline {on
       ? 'bg-slate-200/70 dark:bg-warm-800 text-slate-900 dark:text-warm-50'
       : 'text-slate-600 dark:text-warm-300 hover:text-slate-900 dark:hover:text-warm-50 hover:bg-slate-200/60 dark:hover:bg-warm-800/60'}"
     >
      {#if on}<span class="mark" aria-hidden="true"></span>{/if}
      <s.icon size={15} class="shrink-0 md:hidden {on ? 'text-accent-600 dark:text-accent-300' : ''}" />
      <span class="min-w-0">
       <span class="block text-[13px] font-semibold leading-tight whitespace-nowrap">{s.label}</span>
       <span class="hidden md:block text-[12px] leading-tight mt-0.5 text-slate-500 dark:text-warm-400 truncate">{s.hint}</span>
      </span>
     </a>
    </li>
   {/each}
  </ul>
 </nav>

 <main class="flex-1 min-w-0 flex flex-col overflow-hidden">
  {#if activeSection === 'registry'}
   {#if !namespacesLoaded}
    <div class="mx-auto w-full max-w-5xl px-4 sm:px-6 py-5">
     <div class="leaf h-40 animate-pulse" aria-busy="true" aria-label="Loading registries"></div>
    </div>
   {:else}
    <RegistryListenersPanel {namespaces} canAdmin={canRegistryAdmin} />
   {/if}
  {:else}
   <div class="flex-1 flex items-center justify-center px-4 text-center text-[14px] text-slate-500 dark:text-warm-400">
    You don't have access to any listener settings.
   </div>
  {/if}
 </main>
</div>

<style>
 .mark {
  position: absolute;
  left: 0.75rem;
  right: 0.75rem;
  bottom: 0;
  height: 2px;
  border-radius: 2px;
  background: var(--sec-500);
 }

 @media (min-width: 768px) {
  .mark {
   left: 0;
   right: auto;
   top: 0.375rem;
   bottom: 0.375rem;
   width: 3px;
   height: auto;
  }
 }
</style>
