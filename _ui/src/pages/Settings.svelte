<script lang="ts">
 // /settings/:section — deployment configuration. A left index (a row
 // of tabs on narrow screens) switches between sections; the section
 // lives in the URL so reloads and shared links land in the right place.
 import { link, router } from 'svelte-spa-router';
 import { HardDrive, Info, ExternalLink, Copy, Check, Server, KeyRound, Users, Ticket, LogIn, ShieldCheck } from 'lucide-svelte';
 import { rawMountsStore } from '@/lib/store/rawmounts.svelte';
 import { serveStore } from '@/lib/store/serve.svelte';
 import { appStore } from '@/lib/store/store.svelte';
 import { addToast } from '@/lib/store/toast.svelte';
 import RawMountsPanel from '@/lib/components/settings/RawMountsPanel.svelte';
 import ServePanel from '@/lib/components/settings/ServePanel.svelte';
 import KeyRotationSection from '@/lib/components/settings/KeyRotationSection.svelte';
 import UsersPanel from '@/lib/components/users/UsersPanel.svelte';
 import TokensSection from '@/lib/components/settings/TokensSection.svelte';
 import AuthSection from '@/lib/components/settings/AuthSection.svelte';
 import AccountSecuritySection from '@/lib/components/settings/AccountSecuritySection.svelte';

 // Source repository — derived from the Go module path
 // (github.com/rakunlabs/kutu). Used to build commit/release links.
 const REPO_URL = 'https://github.com/rakunlabs/kutu';

 type Section = 'mounts' | 'serve' | 'encryption' | 'users' | 'tokens' | 'auth' | 'security' | 'about';
 type SectionDef = { key: Section; label: string; hint: string; icon: typeof HardDrive; visible: () => boolean };

 const canSettings = $derived(appStore.hasPermission('settings.manage'));

 // Each section is hidden when the caller lacks the capability behind it.
 const allSections: SectionDef[] = [
  { key: 'mounts', label: 'Raw mounts', hint: 'Storage backends', icon: HardDrive, visible: () => canSettings },
  { key: 'serve', label: 'File serving', hint: 'FTP · SFTP · WebDAV · S3', icon: Server, visible: () => canSettings },
  { key: 'encryption', label: 'Encryption', hint: 'At-rest key', icon: KeyRound, visible: () => canSettings },
  { key: 'users', label: 'Users', hint: 'Users & permissions', icon: Users, visible: () => appStore.hasAnyPermission('users.manage', 'permissions.manage') },
  { key: 'tokens', label: 'Access tokens', hint: 'CLI & registry clients', icon: Ticket, visible: () => appStore.hasPermission('tokens.manage') },
  { key: 'auth', label: 'Authentication', hint: 'Sign-in methods', icon: LogIn, visible: () => canSettings },
  { key: 'security', label: 'Account security', hint: 'Password · 2FA · passkeys', icon: ShieldCheck, visible: () => !!appStore.info?.account_security_available },
  { key: 'about', label: 'About', hint: 'Build information', icon: Info, visible: () => true },
 ];
 const sections = $derived(allSections.filter(s => s.visible()));

 const activeSection = $derived.by<Section>(() => {
  const p = (router.params as Record<string, string> | null | undefined)?.section;
  return sections.some(s => s.key === p) ? (p as Section) : (sections[0]?.key ?? 'about');
 });
 const activeMeta = $derived(sections.find(s => s.key === activeSection)!);

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
   addToast('Could not copy to the clipboard', 'alert');
  }
 }

 // Only users who can see those sections load their data.
 let configLoaded = false;
 $effect(() => {
  if (!canSettings || configLoaded) return;
  configLoaded = true;
  void rawMountsStore.load();
  void serveStore.load();
 });
</script>

<svelte:head><title>{activeMeta.label} · Settings · kutu</title></svelte:head>

<div class="flex flex-col md:flex-row h-full overflow-hidden">
 <!-- Section index: a vertical list on desktop, a scrollable tab row on mobile. -->
 <nav
  aria-label="Settings sections"
  class="shrink-0 md:w-56 border-b md:border-b-0 md:border-r border-slate-300 dark:border-warm-700 bg-slate-50 dark:bg-warm-950/60 overflow-x-auto md:overflow-y-auto"
 >
  <div class="label-caps hidden md:block px-5 pt-5 pb-2 text-[12px] text-slate-500 dark:text-warm-400">Settings</div>
  <ul class="flex md:flex-col gap-0.5 px-2 md:px-3 py-2 md:py-0 md:pb-4 min-w-max md:min-w-0">
   {#each sections as s (s.key)}
    {@const on = activeSection === s.key}
    <li>
     <a
      href={`/settings/${s.key}`}
      use:link
      aria-current={on ? 'page' : undefined}
      class="idx group relative flex items-center gap-2.5 px-3 py-2 rounded-[3px] no-underline {on
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

 <main class="flex-1 min-w-0 overflow-y-auto">
  <div class="mx-auto w-full max-w-5xl px-4 sm:px-8 py-6 sm:py-8">
   {#if activeSection === 'mounts'}
    {#if !rawMountsStore.loaded}
     <div class="leaf h-40 animate-pulse" aria-busy="true" aria-label="Loading raw mounts"></div>
    {:else}
     <RawMountsPanel configs={rawMountsStore.configs} mounts={rawMountsStore.mounts} />
    {/if}
   {:else if activeSection === 'serve'}
    {#if !serveStore.loaded}
     <div class="leaf h-40 animate-pulse" aria-busy="true" aria-label="Loading file serving"></div>
    {:else}
     <ServePanel />
    {/if}
   {:else if activeSection === 'encryption'}
    <KeyRotationSection />
   {:else if activeSection === 'users'}
    <UsersPanel />
   {:else if activeSection === 'tokens'}
    <TokensSection />
   {:else if activeSection === 'auth'}
    <AuthSection />
   {:else if activeSection === 'security'}
    <AccountSecuritySection />
   {:else if activeSection === 'about'}
    <header class="mb-6">
     <h1 class="text-[22px] font-bold tracking-tight text-slate-900 dark:text-warm-50">About</h1>
     <p class="mt-1 text-[14px] text-slate-600 dark:text-warm-300">Build and source information for this kutu instance.</p>
    </header>

    <!-- Title block: labelled cells like the corner of a drawing sheet. -->
    <dl class="leaf grid grid-cols-1 sm:grid-cols-2 overflow-hidden">
     <div class="cell">
      <dt class="label-caps">Name</dt>
      <dd class="text-[15px] font-semibold">{info?.name ?? 'kutu'}</dd>
     </div>
     <div class="cell">
      <dt class="label-caps">Version</dt>
      <dd class="flex items-center gap-2">
       <span class="font-mono text-[14px]">{info?.version ?? 'unknown'}</span>
       {#if hasVersion}
        <a href={versionUrl} target="_blank" rel="noopener noreferrer" class="inline-flex items-center gap-1 text-[12px] font-medium text-accent-700 dark:text-accent-300 hover:underline underline-offset-2">
         Release notes <ExternalLink size={12} />
        </a>
       {:else}
        <span class="tag">dev build</span>
       {/if}
      </dd>
     </div>
     <div class="cell">
      <dt class="label-caps">Commit</dt>
      <dd class="flex items-center gap-2">
       {#if hasCommit}
        <a href={commitUrl} target="_blank" rel="noopener noreferrer" class="font-mono text-[14px] text-accent-700 dark:text-accent-300 hover:underline underline-offset-2" title="View commit on GitHub">{info?.commit}</a>
        <button type="button" class="btn btn-ghost btn-sm btn-icon" onclick={() => copyToClipboard(info?.commit ?? '', 'Commit')} aria-label="Copy commit hash" title="Copy commit hash">
         {#if copied === 'Commit'}<Check size={13} class="text-emerald-600" />{:else}<Copy size={13} />{/if}
        </button>
       {:else}
        <span class="text-slate-500 dark:text-warm-400">Not stamped</span>
       {/if}
      </dd>
     </div>
     <div class="cell">
      <dt class="label-caps">Build date</dt>
      <dd>
       {#if hasDate}<span class="font-mono text-[14px]">{formattedDate}</span>{:else}<span class="text-slate-500 dark:text-warm-400">Not stamped</span>{/if}
      </dd>
     </div>
     <div class="cell sm:col-span-2">
      <dt class="label-caps">Source</dt>
      <dd>
       <a href={REPO_URL} target="_blank" rel="noopener noreferrer" class="inline-flex items-center gap-1.5 text-[14px] text-accent-700 dark:text-accent-300 hover:underline underline-offset-2 break-all">
        {REPO_URL.replace('https://', '')} <ExternalLink size={13} class="shrink-0" />
       </a>
      </dd>
     </div>
    </dl>
   {/if}
  </div>
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

 .cell {
  display: flex;
  flex-direction: column;
  gap: 0.375rem;
  padding: 0.875rem 1.125rem;
  border-bottom: 1px solid var(--color-slate-200);
  min-width: 0;
 }

 .cell dt {
  font-size: 11px;
  color: var(--color-slate-500);
 }

 :global(.dark) .cell {
  border-color: var(--color-warm-700);
 }

 :global(.dark) .cell dt {
  color: var(--color-warm-400);
 }

 @media (min-width: 640px) {
  .cell:nth-child(odd):not(:last-child) {
   border-right: 1px solid var(--color-slate-200);
  }

  :global(.dark) .cell:nth-child(odd):not(:last-child) {
   border-right-color: var(--color-warm-700);
  }
 }

 .cell:last-child {
  border-bottom: 0;
 }
</style>
