<script lang="ts">
  import { tick } from "svelte";
  import { AlertTriangle } from "lucide-svelte";
  import { confirmState, settleConfirm } from "@/lib/store/confirm.svelte";

  let dialog = $state<HTMLDialogElement | null>(null);
  let confirmBtn = $state<HTMLButtonElement | null>(null);
  let cancelBtn = $state<HTMLButtonElement | null>(null);

  const cur = $derived(confirmState.current);

  $effect(() => {
    if (!dialog) return;
    if (cur && !dialog.open) {
      dialog.showModal();
      // Destructive prompts focus Cancel so Enter never deletes by accident.
      tick().then(() => (cur.danger ? cancelBtn : confirmBtn)?.focus());
    } else if (!cur && dialog.open) {
      dialog.close();
    }
  });
</script>

<dialog
  bind:this={dialog}
  class="confirm m-auto w-[min(28rem,calc(100vw-2rem))] p-0 leaf shadow-[0_12px_40px_-8px_rgb(0_0_0/0.35)] backdrop:bg-slate-900/40 dark:backdrop:bg-black/60 text-slate-800 dark:text-warm-100"
  oncancel={(e) => { e.preventDefault(); settleConfirm(false); }}
  onclick={(e) => { if (e.target === dialog) settleConfirm(false); }}
  aria-labelledby="confirm-title"
>
  {#if cur}
    <div class="flex gap-3 px-5 pt-5 pb-4">
      {#if cur.danger}
        <AlertTriangle size={18} class="shrink-0 mt-0.5 text-vermilion-600 dark:text-vermilion-300" />
      {/if}
      <div class="min-w-0">
        <h2 id="confirm-title" class="text-[15px] font-semibold leading-snug">{cur.title}</h2>
        {#if cur.message}
          <p class="mt-1.5 text-[13px] leading-relaxed text-slate-600 dark:text-warm-300 whitespace-pre-line">{cur.message}</p>
        {/if}
      </div>
    </div>
    <div class="flex justify-end gap-2 px-5 py-3 border-t border-slate-200 dark:border-warm-700 bg-slate-50 dark:bg-warm-900/60">
      <button bind:this={cancelBtn} type="button" class="btn btn-secondary" onclick={() => settleConfirm(false)}>
        {cur.cancelLabel ?? "Cancel"}
      </button>
      <button
        bind:this={confirmBtn}
        type="button"
        class="btn {cur.danger ? 'btn-danger' : 'btn-primary'}"
        onclick={() => settleConfirm(true)}
      >
        {cur.confirmLabel ?? "Confirm"}
      </button>
    </div>
  {/if}
</dialog>
