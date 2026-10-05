// Promise-based replacement for window.confirm(), rendered by
// <ConfirmDialog /> at the app root.

export interface ConfirmOptions {
  title: string;
  message?: string;
  confirmLabel?: string;
  cancelLabel?: string;
  danger?: boolean;
}

interface Pending extends ConfirmOptions {
  resolve: (ok: boolean) => void;
}

export const confirmState = $state<{ current: Pending | null }>({ current: null });

export function confirmAction(opts: ConfirmOptions): Promise<boolean> {
  // Resolve any dialog still open as cancelled before replacing it.
  confirmState.current?.resolve(false);
  return new Promise((resolve) => {
    confirmState.current = { ...opts, resolve };
  });
}

export function settleConfirm(ok: boolean) {
  const cur = confirmState.current;
  confirmState.current = null;
  cur?.resolve(ok);
}
