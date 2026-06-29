<script lang="ts">
  import Router from "svelte-spa-router";
  import Navbar from "@/lib/components/Navbar.svelte";
  import Toast from "@/lib/components/Toast.svelte";
  import UnlockScreen from "@/lib/components/UnlockScreen.svelte";
  import routes from "@/routes";
  import { appStore } from "@/lib/store/store.svelte";
  import { keymgrStore } from "@/lib/store/keymgr.svelte";

  // Boot: load server info + at-rest key status in parallel. kutu has
  // no login, so there's no identity/auth round-trip — the shell renders
  // immediately (or the unlock takeover when the server is locked).
  $effect.root(() => {
    appStore.loadInfo();
    keymgrStore.refreshStatus();
  });

  // Server-lock takeover: only when encryption was opted into
  // (initialized) AND this process hasn't been unlocked yet. status ===
  // null (not fetched) is treated as "not locked" to avoid a flash.
  const serverLocked = $derived(
    keymgrStore.status !== null &&
      keymgrStore.status.initialized &&
      !keymgrStore.status.unlocked,
  );
</script>

<Toast />

{#if serverLocked}
  <UnlockScreen />
{:else}
  <div
    class="flex flex-col h-full w-full overflow-hidden bg-slate-100 dark:bg-warm-900 text-slate-800 dark:text-slate-100"
  >
    <Navbar />
    <div class="flex-1 overflow-hidden">
      <Router {routes} />
    </div>
  </div>
{/if}
