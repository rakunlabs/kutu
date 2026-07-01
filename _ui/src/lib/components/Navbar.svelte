<script lang="ts">
  import { tick } from "svelte";
  import { link } from "svelte-spa-router";
  import active from "svelte-spa-router/active";
  import { Boxes, FolderTree, Lock, Settings, User } from "lucide-svelte";
  import ThemeSwitcher from "@/lib/components/ThemeSwitcher.svelte";
  import { appStore } from "@/lib/store/store.svelte";
  import { userStore } from "@/lib/store/user.svelte";

  const nav = [
    { href: "/registries", match: /^\/(registries)?$/, label: "Registries", icon: Boxes },
    { href: "/files", match: "/files", label: "Files", icon: FolderTree },
    { href: "/settings", match: "/settings", label: "Settings", icon: Settings },
  ];

  // X-User pill: click to edit the audit actor sent with every request.
  let editing = $state(false);
  let draft = $state("");
  let inputEl = $state<HTMLInputElement | null>(null);

  async function startEdit() {
    draft = userStore.user;
    editing = true;
    await tick();
    inputEl?.focus();
    inputEl?.select();
  }

  function commit() {
    userStore.setUser(draft);
    editing = false;
  }

  function onKeydown(e: KeyboardEvent) {
    if (e.key === "Enter") {
      commit();
    } else if (e.key === "Escape") {
      // Reset the draft so the commit-on-blur is a no-op.
      draft = userStore.user;
      editing = false;
    }
  }
</script>

<header
  class="flex items-center gap-1 h-10 bg-warm-900 text-white border-b border-warm-700 px-4 shrink-0"
>
  <a
    href="/"
    use:link
    class="flex items-center gap-2 mr-6 font-bold tracking-wide text-white"
  >
    <Boxes size={18} color="#EF233C" />
    <span class="text-sm">kutu</span>
  </a>

  <nav class="flex items-center gap-1">
    {#each nav as item (item.href)}
      {@const Icon = item.icon}
      <a
        href={item.href}
        use:link
        use:active={{ path: item.match, className: "nav-active" }}
        class="nav-link flex items-center gap-1.5 rounded px-3 py-1.5 text-xs font-medium no-underline transition-colors text-warm-200 hover:text-white hover:bg-warm-700"
      >
        <Icon size={14} />
        {item.label}
      </a>
    {/each}
  </nav>

  <div class="ml-auto flex items-center gap-2 text-warm-300">
    {#if appStore.info?.key_initialized}
      <span
        class="flex items-center gap-1 text-xs {appStore.info?.key_unlocked
          ? 'text-emerald-400'
          : 'text-amber-400'}"
        title={appStore.info?.key_unlocked ? "encryption unlocked" : "encryption locked"}
      >
        <Lock size={13} />
      </span>
    {/if}

    <!-- X-User: the audit actor stamped on every mutation. Editable
         because kutu has no login. -->
    {#if editing}
      <input
        bind:this={inputEl}
        bind:value={draft}
        onkeydown={onKeydown}
        onblur={commit}
        placeholder="username"
        class="w-32 px-2 py-1 rounded text-xs font-medium bg-warm-800 text-warm-100 border border-warm-600 placeholder-warm-400 focus:outline-none focus:ring-1 focus:ring-accent-500"
      />
    {:else}
      <button
        type="button"
        onclick={startEdit}
        title="Set the X-User sent with requests (audit attribution)"
        class="flex items-center gap-1.5 px-3 py-1.5 rounded text-xs font-medium bg-warm-700 hover:bg-warm-600 cursor-pointer {userStore.user
          ? 'text-warm-100'
          : 'text-warm-400'}"
      >
        <User size={14} />
        {userStore.user || "Set user"}
      </button>
    {/if}

    <ThemeSwitcher variant="dark" />
  </div>
</header>

<style>
  /* Active nav entry — pika's filled-accent selection (bg-accent-600,
     white text). The navbar is a permanently-dark warm-900 bar, so no
     light-mode variant is needed. */
  :global(.nav-link.nav-active) {
    background-color: var(--color-accent-600);
    color: #fff;
  }
</style>
