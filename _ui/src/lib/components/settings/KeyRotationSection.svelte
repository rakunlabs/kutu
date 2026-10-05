<script lang="ts">
  import { onMount } from "svelte";
  import { addToast } from "@/lib/store/toast.svelte";
  import {
    Eye,
    EyeOff,
    RotateCw,
    AlertTriangle,
    Lock,
    KeyRound,
  } from "lucide-svelte";
  import { keymgrStore } from "@/lib/store/keymgr.svelte";
  import { confirmAction } from "@/lib/store/confirm.svelte";
  import PanelHeader from "./PanelHeader.svelte";

  // Server-key management panel. Two modes driven by
  // keymgrStore.status.initialized:
  //
  //   - Not initialized: render an opt-in form. Choosing a key
  //     here writes the verifier and unlocks the server in one
  //     step. From this point on every restart requires unlock.
  //
  //   - Initialized: render rotate + lock-now. The full-screen
  //     unlock takeover handles the every-restart unlock flow; this
  //     panel is just the steady-state admin surface.
  //
  // We deliberately do NOT auto-redirect after initialize. Once the
  // verifier is on disk the operator can keep working (the manager
  // is left unlocked by the initialize call). On the NEXT restart
  // the unlock screen will appear.

  // Status refresh on mount so that opening Settings → this panel
  // never shows stale data from an earlier route.
  onMount(() => {
    keymgrStore.refreshStatus();
  });

  const status = $derived(keymgrStore.status);
  const initialized = $derived(status?.initialized === true);
  const busy = $derived(keymgrStore.busy);

  // ─── Initialize form ───────────────────────────────────────
  let initKey = $state("");
  let initConfirm = $state("");
  let showInit = $state(false);

  // ─── Rotate form ───────────────────────────────────────────
  let currentKey = $state("");
  let newKey = $state("");
  let confirmKey = $state("");
  let showCurrent = $state(false);
  let showNew = $state(false);

  // Local validation surface — kept separate from
  // keymgrStore.error so a typo here doesn't override the
  // server-side rejection message.
  let localError = $state<string | null>(null);

  function clearLocal() {
    localError = null;
    keymgrStore.setError(null);
  }

  async function onInitialize(e: Event) {
    e.preventDefault();
    if (!initKey) {
      localError = "Key is required";
      return;
    }
    if (initKey !== initConfirm) {
      localError = "Keys do not match";
      return;
    }
    localError = null;
    const ok = await keymgrStore.initialize(initKey);
    if (ok) {
      addToast(
        "Server encryption enabled. Save the key — every restart will require it.",
        "success",
        8000,
      );
      initKey = "";
      initConfirm = "";
    }
  }

  async function onRotate(e: Event) {
    e.preventDefault();
    if (!currentKey) {
      localError = "Current key is required";
      return;
    }
    if (!newKey) {
      localError = "New key is required";
      return;
    }
    if (newKey === currentKey) {
      localError = "New key must differ from current key";
      return;
    }
    if (newKey !== confirmKey) {
      localError = "New key and confirmation do not match";
      return;
    }
    localError = null;

    const ok = await keymgrStore.rotate(currentKey, newKey);
    if (ok) {
      addToast(
        "Server key rotated. Save the new key — restarts will require it.",
        "success",
        6000,
      );
      currentKey = "";
      newKey = "";
      confirmKey = "";
    }
  }

  async function onLockNow() {
    const sure = await confirmAction({
      title: "Lock the server now?",
      message:
        "The key is cleared from memory. Everyone, including you, sees the unlock screen until someone enters the key again.",
      confirmLabel: "Lock server",
      danger: true,
    });
    if (!sure) return;
    const ok = await keymgrStore.lock();
    if (ok) {
      addToast(
        "Server locked. The next request will redirect to unlock.",
        "success",
        4000,
      );
    }
  }
</script>

{#snippet secret(id: string, label: string, value: string, set: (v: string) => void, shown: boolean, toggle: (() => void) | null, autocomplete: "new-password" | "current-password")}
  <label class="field" for={id}>
    <span class="field-label">{label}</span>
    <span class="relative flex">
      <input
        {id}
        type={shown ? "text" : "password"}
        {value}
        oninput={(e) => { set(e.currentTarget.value); clearLocal(); }}
        {autocomplete}
        disabled={busy}
        class="input font-mono {toggle ? 'pr-9' : ''}"
      />
      {#if toggle}
        <button
          type="button"
          onclick={toggle}
          class="btn btn-ghost btn-sm btn-icon absolute right-0.5 top-1/2 -translate-y-1/2"
          aria-label={shown ? `Hide ${label.toLowerCase()}` : `Show ${label.toLowerCase()}`}
        >
          {#if shown}<EyeOff size={14} />{:else}<Eye size={14} />{/if}
        </button>
      {/if}
    </span>
  </label>
{/snippet}

{#snippet warning(text: string)}
  <div class="flex items-start gap-2.5 px-3.5 py-3 rounded-[3px] bg-amber-50 text-amber-900 dark:bg-amber-950/40 dark:text-amber-100 text-[13px] leading-relaxed">
    <AlertTriangle size={15} class="shrink-0 mt-0.5 text-amber-600 dark:text-amber-300" />
    <span>{text}</span>
  </div>
{/snippet}

{#snippet errorBox()}
  {#if localError || keymgrStore.error}
    <p class="text-[13px] text-vermilion-700 dark:text-vermilion-300" role="alert">{localError || keymgrStore.error}</p>
  {/if}
{/snippet}

<PanelHeader title="Encryption">
  The at-rest key seals mount credentials, file-server secrets and other sensitive settings in the database.
</PanelHeader>

<div class="leaf flex flex-wrap items-center gap-x-6 gap-y-2 px-4 py-3.5 mb-8">
  <span class="label-caps text-[11px] text-slate-500 dark:text-warm-400">State</span>
  {#if status === null}
    <span class="status status-off">Checking…</span>
  {:else if !initialized}
    <span class="status status-off">Not enabled</span>
    <span class="text-[13px] text-slate-600 dark:text-warm-300">Secrets are stored without at-rest encryption.</span>
  {:else}
    <span class="status status-ok">Enabled and unlocked</span>
    <span class="text-[13px] text-slate-600 dark:text-warm-300">Every restart asks for the key.</span>
  {/if}
</div>

{#if !initialized}
  <section>
    <PanelHeader title="Enable encryption" level={2}>
      Choose a master key. From then on, kutu starts locked after every restart until someone enters it.
    </PanelHeader>
    <form onsubmit={onInitialize} class="leaf p-5 flex flex-col gap-4 max-w-xl">
      {@render warning("Store the key in a password manager before you enable. A lost key makes the encrypted data unrecoverable; there is no reset.")}
      {@render secret("init-key", "Master key", initKey, (v) => (initKey = v), showInit, () => (showInit = !showInit), "new-password")}
      {@render secret("init-confirm", "Repeat master key", initConfirm, (v) => (initConfirm = v), showInit, null, "new-password")}
      {@render errorBox()}
      <div class="flex justify-end">
        <button type="submit" class="btn btn-primary" disabled={busy || !initKey || !initConfirm}>
          <KeyRound size={14} /> {busy ? "Enabling…" : "Enable encryption"}
        </button>
      </div>
    </form>
  </section>
{:else}
  <div class="flex flex-col gap-10">
    <section>
      <PanelHeader title="Rotate key" level={2}>
        Re-encrypts every stored secret with a new key. The next restart needs the new key.
      </PanelHeader>
      <form onsubmit={onRotate} class="leaf p-5 flex flex-col gap-4 max-w-xl">
        {@render warning("Save the new key before rotating. After a restart, only the new key unlocks the server.")}
        {@render secret("rotate-current", "Current key", currentKey, (v) => (currentKey = v), showCurrent, () => (showCurrent = !showCurrent), "current-password")}
        {@render secret("rotate-new", "New key", newKey, (v) => (newKey = v), showNew, () => (showNew = !showNew), "new-password")}
        {@render secret("rotate-confirm", "Repeat new key", confirmKey, (v) => (confirmKey = v), showNew, null, "new-password")}
        {@render errorBox()}
        <div class="flex justify-end">
          <button type="submit" class="btn btn-primary" disabled={busy || !currentKey || !newKey || !confirmKey}>
            <RotateCw size={14} class={busy ? "animate-spin" : ""} /> {busy ? "Rotating…" : "Rotate key"}
          </button>
        </div>
      </form>
    </section>

    <section>
      <PanelHeader title="Lock now" level={2}>
        Clears the key from memory without restarting. Useful before handing over or stepping away.
      </PanelHeader>
      <div class="leaf p-5 flex flex-wrap items-center justify-between gap-4 max-w-xl">
        <span class="text-[13px] text-slate-600 dark:text-warm-300">Everyone is sent to the unlock screen.</span>
        <button type="button" onclick={onLockNow} disabled={busy} class="btn btn-secondary">
          <Lock size={14} /> Lock server
        </button>
      </div>
    </section>
  </div>
{/if}
