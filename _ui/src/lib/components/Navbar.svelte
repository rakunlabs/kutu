<script lang="ts">
  import { link, router, push } from "svelte-spa-router";
  import { Boxes, ChevronDown, FolderTree, Lock, LockOpen, LogOut, RadioTower, Settings, ShieldCheck, User } from "lucide-svelte";
  import ThemeSwitcher from "@/lib/components/ThemeSwitcher.svelte";
  import { appStore } from "@/lib/store/store.svelte";

  // Index tabs. Each section owns one hue; the active tab steps down
  // into the section board strip below the header (the signature move).
  // Settings is open to everyone because account security lives there.
  const allNav = [
    { href: "/registries", key: "registries", label: "Registries", icon: Boxes, hue: "var(--color-oxide)", perm: "registry.read" },
    { href: "/files", key: "files", label: "Files", icon: FolderTree, hue: "var(--color-teal)", perm: "raw.read" },
    { href: "/listeners", key: "listeners", label: "Listeners", icon: RadioTower, hue: "var(--color-plum)", perm: "registry.admin" },
    { href: "/settings", key: "settings", label: "Settings", icon: Settings, hue: "var(--color-ultra)", perm: "" },
  ];
  const nav = $derived(allNav.filter((n) => !n.perm || appStore.hasPermission(n.perm)));

  const current = $derived.by(() => {
    const loc = router.location ?? "/";
    if (loc.startsWith("/files")) return "files";
    if (loc.startsWith("/listeners")) return "listeners";
    if (loc.startsWith("/settings")) return "settings";
    return "registries";
  });

  const username = $derived(appStore.info?.user || appStore.identity?.name || appStore.identity?.subject || "Account");

  let menuOpen = $state(false);
  let menuEl = $state<HTMLDivElement | null>(null);

  function onWindowClick(e: MouseEvent) {
    if (menuOpen && menuEl && !menuEl.contains(e.target as Node)) menuOpen = false;
  }

  function onWindowKey(e: KeyboardEvent) {
    if (menuOpen && e.key === "Escape") menuOpen = false;
  }

  function goSecurity() {
    menuOpen = false;
    push("/settings/security");
  }

  async function signOut() {
    menuOpen = false;
    await appStore.logout();
  }
</script>

<svelte:window onclick={onWindowClick} onkeydown={onWindowKey} />

<header class="shrink-0 bg-warm-950 text-warm-100">
  <div class="flex items-end h-12 px-3 sm:px-4 gap-2">
    <a href="/" use:link class="flex items-center gap-2 self-center mr-2 sm:mr-5 text-warm-50" aria-label="kutu home">
      <Boxes size={18} color="#EF233C" />
      <span class="text-[15px] font-bold tracking-wide" style="font-stretch: 112%">kutu</span>
    </a>

    <nav class="flex items-end gap-1 h-full overflow-hidden" aria-label="Sections">
      {#each nav as item (item.href)}
        {@const Icon = item.icon}
        {@const on = current === item.key}
        <a
          href={item.href}
          use:link
          aria-current={on ? "page" : undefined}
          class="tab label-caps flex items-center gap-1.5 px-3 sm:px-3.5 text-[12px] rounded-t-[3px] no-underline {on ? 'is-on' : ''}"
          style="--hue: {item.hue}"
        >
          <Icon size={14} class="shrink-0" />
          <span class="sr-only sm:not-sr-only">{item.label}</span>
        </a>
      {/each}
    </nav>

    <div class="ml-auto flex items-center gap-2 self-center">
      {#if appStore.info?.key_initialized}
        {@const unlocked = appStore.info?.key_unlocked}
        <span
          class="hidden md:inline-flex items-center gap-1.5 text-[12px] font-medium {unlocked ? 'text-emerald-300' : 'text-amber-300'}"
          title={unlocked ? "At-rest encryption is unlocked" : "At-rest encryption is locked"}
        >
          {#if unlocked}<LockOpen size={13} />{:else}<Lock size={13} />{/if}
          {unlocked ? "Unlocked" : "Locked"}
        </span>
      {/if}

      <div class="relative" bind:this={menuEl}>
        <button
          type="button"
          onclick={() => (menuOpen = !menuOpen)}
          aria-haspopup="menu"
          aria-expanded={menuOpen}
          class="flex items-center gap-1.5 h-7 px-2.5 rounded-[3px] text-[12px] font-medium border border-warm-700 hover:border-warm-400 text-warm-50 cursor-pointer"
        >
          <User size={13} />
          <span class="hidden sm:inline max-w-32 truncate">{username}</span>
          {#if appStore.info?.is_superadmin}
            <span class="hidden sm:inline-flex label-caps text-[11px] leading-none px-1 py-0.5 rounded-[2px] border border-warm-500 text-warm-200">superadmin</span>
          {/if}
          <ChevronDown size={12} class="text-warm-300" />
        </button>
        {#if menuOpen}
          <div
            role="menu"
            class="menu absolute right-0 top-full mt-1.5 z-50 min-w-48 py-1 rounded-[3px] border border-slate-300 dark:border-warm-700 bg-white dark:bg-warm-800 text-slate-800 dark:text-warm-100"
          >
            <div class="px-3 py-2 border-b border-slate-200 dark:border-warm-700">
              <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400">Signed in as</div>
              <div class="text-[13px] font-semibold truncate">{username}</div>
              {#if appStore.info?.is_superadmin}<span class="tag mt-1">superadmin</span>{/if}
            </div>
            {#if appStore.info?.account_security_available}
              <button type="button" role="menuitem" class="item" onclick={goSecurity}>
                <ShieldCheck size={14} /> Account security
              </button>
            {/if}
            <button type="button" role="menuitem" class="item" onclick={signOut}>
              <LogOut size={14} /> Sign out
            </button>
          </div>
        {/if}
      </div>

      <ThemeSwitcher variant="dark" />
    </div>
  </div>
  <!-- The section board: the active tab's hue, full width. -->
  <div class="h-2" style="background: var(--sec-500)"></div>
</header>

<style>
  .menu {
    box-shadow: 0 8px 24px -6px rgb(0 0 0 / 0.3);
  }

  .item {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    width: 100%;
    padding: 0.5rem 0.75rem;
    font-size: 13px;
    text-align: left;
    cursor: pointer;
  }

  .item:hover {
    background: var(--color-slate-100);
  }

  :global(.dark) .item:hover {
    background: var(--color-warm-700);
  }

  .tab {
    height: 2.5rem;
    margin-bottom: -1px;
    color: var(--color-warm-300);
    border: 1px solid transparent;
    border-bottom: 0;
    transform: translateY(0.375rem);
    transition: transform 90ms steps(2), background-color 90ms steps(2), color 90ms steps(2);
  }

  .tab:hover {
    color: var(--color-warm-50);
    background: color-mix(in oklab, var(--hue) 22%, transparent);
  }

  /* Active: the tab steps up and becomes the section board. */
  .tab.is-on {
    transform: translateY(0);
    background: var(--hue);
    color: #fff;
  }
</style>
