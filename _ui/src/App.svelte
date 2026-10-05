<script lang="ts">
  import Router, { router, replace } from "svelte-spa-router";
  import { Loader2 } from "lucide-svelte";
  import Navbar from "@/lib/components/Navbar.svelte";
  import Toast from "@/lib/components/Toast.svelte";
  import ConfirmDialog from "@/lib/components/ConfirmDialog.svelte";
  import UnlockScreen from "@/lib/components/UnlockScreen.svelte";
  import Login from "@/pages/Login.svelte";
  import routes from "@/routes";
  import { appStore } from "@/lib/store/store.svelte";
  import { keymgrStore } from "@/lib/store/keymgr.svelte";

  // Boot: server info, identity and at-rest key status in parallel. The
  // key status is reachable while the server is locked; the others
  // degrade gracefully.
  $effect.root(() => {
    appStore.loadInfo();
    appStore.loadIdentity();
    keymgrStore.refreshStatus();
  });

  const authenticated = $derived(appStore.authenticated);

  // Server-lock takeover: only when encryption was opted into
  // (initialized) AND this process hasn't been unlocked yet. status ===
  // null (not fetched) is treated as "not locked" to avoid a flash.
  const serverLocked = $derived(
    keymgrStore.status !== null &&
      keymgrStore.status.initialized &&
      !keymgrStore.status.unlocked,
  );

  // Send users away from sections they can't open (Registries is the
  // default route) to the first one they can.
  $effect(() => {
    if (authenticated !== true || serverLocked || appStore.info === null) return;
    const loc = router.location ?? "/";
    const inRegistries = loc === "/" || loc.startsWith("/registries");
    const inFiles = loc.startsWith("/files");
    const inListeners = loc.startsWith("/listeners");
    if (
      (inRegistries && !appStore.hasPermission("registry.read")) ||
      (inFiles && !appStore.hasPermission("raw.read")) ||
      (inListeners && !appStore.hasPermission("registry.admin"))
    ) {
      const target = appStore.hasPermission("registry.read")
        ? "/registries"
        : appStore.hasPermission("raw.read")
          ? "/files"
          : "/settings";
      if (!loc.startsWith(target)) replace(target);
    }
  });

  // The section decides the accent hue for everything beneath it
  // (see [data-section] in global.css).
  const section = $derived.by(() => {
    const loc = router.location ?? "/";
    if (loc.startsWith("/files")) return "files";
    if (loc.startsWith("/listeners")) return "listeners";
    if (loc.startsWith("/settings")) return "settings";
    return "registries";
  });
</script>

<Toast />

{#if authenticated === null}
  <div
    class="flex items-center justify-center h-full w-full bg-slate-100 dark:bg-warm-900 text-slate-500 dark:text-warm-400"
    aria-busy="true"
  >
    <Loader2 size={18} class="animate-spin" />
    <span class="sr-only">Loading</span>
  </div>
{:else if authenticated === false}
  <Login />
{:else if serverLocked}
  <UnlockScreen />
{:else}
  <div
    data-section={section}
    class="flex flex-col h-full w-full overflow-hidden bg-slate-100 dark:bg-warm-900 text-slate-800 dark:text-warm-100"
  >
    <ConfirmDialog />
    <Navbar />
    <div class="flex-1 overflow-hidden">
      <Router {routes} />
    </div>
  </div>
{/if}
