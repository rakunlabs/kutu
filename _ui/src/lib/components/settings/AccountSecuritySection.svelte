<script lang="ts">
 // Settings → Account security: the signed-in user's own password,
 // TOTP second factor and passkeys. Endpoints live under /api/v1/me/*
 // and are available to every user while account_security_available.
 import axios from 'axios';
 import { onMount } from 'svelte';
 import QRCode from 'qrcode';
 import {
  Key,
  Plus,
  Trash2,
  Pencil,
  Smartphone,
  Usb,
  Wifi,
  Globe,
  Check,
  X,
  Copy,
  RefreshCw,
  Eye,
  EyeOff,
 } from 'lucide-svelte';
 import { apiErrorMessage, apiErrorStatus } from '@/lib/api/client';
 import { appStore } from '@/lib/store/store.svelte';
 import { addToast } from '@/lib/store/toast.svelte';
 import { confirmAction } from '@/lib/store/confirm.svelte';
 import PanelHeader from './PanelHeader.svelte';
 import { isWebAuthnSupported, startRegistration, type ServerCreationOptions } from '@/lib/webauthn';

 interface PasskeyCredential {
  id: string;
  user_id: string;
  credential_id: string;
  aaguid?: string;
  sign_count: number;
  transports?: string[];
  user_verified: boolean;
  backup_eligible: boolean;
  backup_state: boolean;
  attestation_type?: string;
  name: string;
  created_at: string;
  last_used_at?: string;
 }

 interface TOTPStatus {
  enabled: boolean;
  created_at?: string;
  last_used_at?: string;
  recovery_codes_left: number;
  pending_enrollment: boolean;
 }

 interface TOTPEnrollment {
  secret_base32: string;
  otpauth_url: string;
  period: number;
  digits: number;
  algorithm: string;
 }

 // Password change only applies to accounts with a kutu password. An
 // identity from an OAuth2 provider has no local password to change.
 const isLocalAccount = $derived.by(() => {
  const provider = appStore.identity?.provider ?? '';
  const localName = appStore.info?.local_login_name ?? '';
  return !provider || provider === localName || provider === 'local' || provider === 'passkey';
 });

 // ── Password ──
 let currentPassword = $state('');
 let newPassword = $state('');
 let confirmPassword = $state('');
 let showPasswords = $state(false);
 let passwordBusy = $state(false);
 let passwordError = $state('');

 const passwordProblem = $derived.by(() => {
  if (!currentPassword || !newPassword) return null;
  if (newPassword === currentPassword) return 'The new password matches the current one.';
  if (confirmPassword && confirmPassword !== newPassword) return 'The two new passwords differ.';
  return null;
 });

 async function changePassword(e: Event) {
  e.preventDefault();
  passwordError = '';
  if (!currentPassword || !newPassword || newPassword !== confirmPassword || passwordProblem) return;
  passwordBusy = true;
  try {
   await axios.post('/api/v1/me/password', { current_password: currentPassword, new_password: newPassword });
   currentPassword = '';
   newPassword = '';
   confirmPassword = '';
   addToast('Password changed', 'success');
  } catch (err) {
   passwordError = apiErrorStatus(err) === 401
    ? 'The current password is wrong.'
    : apiErrorMessage(err, 'Could not change the password');
  } finally {
   passwordBusy = false;
  }
 }

 // ── Passkeys ──
 let passkeys = $state<PasskeyCredential[]>([]);
 let loading = $state(true);
 let loadError = $state('');
 let webauthnSupported = $state(true);
 let enrolling = $state(false);
 let newName = $state('');
 let newAttachment = $state<'' | 'platform' | 'cross-platform'>('');
 let renamingID = $state<string | null>(null);
 let renameDraft = $state('');

 // ── TOTP ──
 let totpStatus = $state<TOTPStatus | null>(null);
 let totpLoading = $state(true);
 let totpLoadError = $state('');
 let enrollment = $state<TOTPEnrollment | null>(null);
 let qrDataURI = $state('');
 let enrollmentCode = $state('');
 let enrollmentBusy = $state(false);
 // Shown once after enrollment / regenerate; never refetched.
 let lastRecoveryCodes = $state<string[]>([]);
 let disablePassword = $state('');
 let disableBusy = $state(false);
 let showDisable = $state(false);
 let regenPassword = $state('');
 let regenBusy = $state(false);
 let showRegen = $state(false);

 onMount(async () => {
  webauthnSupported = isWebAuthnSupported();
  await Promise.all([loadPasskeys(), loadTOTPStatus()]);
 });

 async function loadTOTPStatus() {
  totpLoading = true;
  totpLoadError = '';
  try {
   const res = await axios.get<TOTPStatus>('/api/v1/me/totp');
   totpStatus = res.data;
  } catch (err) {
   totpStatus = null;
   totpLoadError = apiErrorStatus(err) === 503
    ? 'Two-factor authentication is not available on this server.'
    : apiErrorMessage(err, 'Could not load the two-factor status');
  } finally {
   totpLoading = false;
  }
 }

 async function startTOTPEnrollment() {
  enrollmentBusy = true;
  try {
   const res = await axios.post<TOTPEnrollment>('/api/v1/me/totp/begin', {});
   enrollment = res.data;
   qrDataURI = await QRCode.toDataURL(res.data.otpauth_url, { margin: 1, width: 216, errorCorrectionLevel: 'M' });
   enrollmentCode = '';
   await loadTOTPStatus();
  } catch (err) {
   addToast(apiErrorMessage(err, 'Could not start enrollment'), 'alert');
  } finally {
   enrollmentBusy = false;
  }
 }

 function cancelEnrollment() {
  enrollment = null;
  qrDataURI = '';
  enrollmentCode = '';
 }

 async function finishTOTPEnrollment(e?: Event) {
  e?.preventDefault();
  if (!enrollment) return;
  const code = enrollmentCode.trim();
  if (!/^\d{6}$/.test(code)) {
   addToast('Enter the 6-digit code from your authenticator app', 'alert');
   return;
  }
  enrollmentBusy = true;
  try {
   const res = await axios.post<{ recovery_codes: string[] }>('/api/v1/me/totp/finish', { code });
   lastRecoveryCodes = res.data.recovery_codes ?? [];
   addToast('Two-factor authentication is on. Save your recovery codes.', 'success');
   cancelEnrollment();
   await loadTOTPStatus();
  } catch (err) {
   addToast(apiErrorMessage(err, 'Verification failed'), 'alert');
  } finally {
   enrollmentBusy = false;
  }
 }

 async function disableTOTP(e?: Event) {
  e?.preventDefault();
  if (!disablePassword) return;
  disableBusy = true;
  try {
   await axios.delete('/api/v1/me/totp', { data: { password: disablePassword } });
   addToast('Two-factor authentication turned off', 'success');
   disablePassword = '';
   showDisable = false;
   await loadTOTPStatus();
  } catch (err) {
   addToast(apiErrorMessage(err, 'Could not turn off two-factor authentication'), 'alert');
  } finally {
   disableBusy = false;
  }
 }

 async function regenerateRecoveryCodes(e?: Event) {
  e?.preventDefault();
  if (!regenPassword) return;
  regenBusy = true;
  try {
   const res = await axios.post<{ recovery_codes: string[] }>('/api/v1/me/totp/recovery-codes', { password: regenPassword });
   lastRecoveryCodes = res.data.recovery_codes ?? [];
   addToast('New recovery codes generated. Save them now.', 'success');
   regenPassword = '';
   showRegen = false;
   await loadTOTPStatus();
  } catch (err) {
   addToast(apiErrorMessage(err, 'Could not regenerate the codes'), 'alert');
  } finally {
   regenBusy = false;
  }
 }

 // navigator.clipboard needs a secure context; fall back on plain http.
 async function copyToClipboard(text: string, label: string) {
  try {
   if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(text);
   } else {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    document.execCommand('copy');
    document.body.removeChild(ta);
   }
   addToast(`${label} copied`, 'success');
  } catch {
   addToast(`Could not copy the ${label.toLowerCase()}`, 'alert');
  }
 }

 async function loadPasskeys() {
  loading = true;
  loadError = '';
  try {
   const res = await axios.get<PasskeyCredential[]>('/api/v1/me/passkeys');
   passkeys = Array.isArray(res.data) ? res.data : [];
  } catch (err) {
   passkeys = [];
   loadError = apiErrorStatus(err) === 503
    ? 'Passkeys are not enabled on this server. An administrator can turn them on under Authentication.'
    : apiErrorMessage(err, 'Could not load passkeys');
  } finally {
   loading = false;
  }
 }

 async function handleEnroll(e?: Event) {
  e?.preventDefault();
  if (!webauthnSupported) {
   addToast('This browser does not support passkeys.', 'alert');
   return;
  }
  enrolling = true;
  try {
   const beginBody: { name: string; attachment?: string } = { name: newName.trim() };
   if (newAttachment) beginBody.attachment = newAttachment;
   const beginRes = await axios.post<{ session_id: string; options: ServerCreationOptions }>('/api/v1/me/passkeys/begin', beginBody);
   const { session_id, options } = beginRes.data;
   const response = await startRegistration(options);
   const finishRes = await axios.post<PasskeyCredential>('/api/v1/me/passkeys/finish', { session_id, name: newName.trim(), response });
   passkeys = [finishRes.data, ...passkeys];
   addToast(`Passkey "${finishRes.data.name}" added`, 'success');
   newName = '';
   newAttachment = '';
  } catch (err: any) {
   const code = err?.name ?? '';
   if (code === 'NotAllowedError') addToast('Enrollment cancelled', 'info');
   else if (code === 'InvalidStateError') addToast('That device is already enrolled', 'alert');
   else addToast(apiErrorMessage(err, 'Enrollment failed'), 'alert');
  } finally {
   enrolling = false;
  }
 }

 function startRename(p: PasskeyCredential) {
  renamingID = p.id;
  renameDraft = p.name;
 }

 function cancelRename() {
  renamingID = null;
  renameDraft = '';
 }

 async function saveRename(p: PasskeyCredential) {
  const name = renameDraft.trim();
  if (!name || name === p.name) {
   cancelRename();
   return;
  }
  try {
   const res = await axios.patch<PasskeyCredential>(`/api/v1/me/passkeys/${p.id}`, { name });
   passkeys = passkeys.map((x) => (x.id === p.id ? res.data : x));
   addToast('Passkey renamed', 'success');
  } catch (err) {
   addToast(apiErrorMessage(err, 'Could not rename the passkey'), 'alert');
  } finally {
   cancelRename();
  }
 }

 async function handleDelete(p: PasskeyCredential) {
  const isLastOne = passkeys.length === 1;
  const ok = await confirmAction({
   title: `Delete passkey "${p.name}"?`,
   message: isLastOne
    ? "This is your only passkey. Afterwards you need another way to sign in (password or single sign-on), or you'll be locked out."
    : 'You can no longer sign in with it. Your other passkeys keep working.',
   confirmLabel: 'Delete passkey',
   danger: true,
  });
  if (!ok) return;
  try {
   await axios.delete(`/api/v1/me/passkeys/${p.id}`);
   passkeys = passkeys.filter((x) => x.id !== p.id);
   addToast(`Passkey "${p.name}" deleted`, 'success');
  } catch (err) {
   addToast(apiErrorMessage(err, 'Could not delete the passkey'), 'alert');
  }
 }

 function transportIcon(transports?: string[]): typeof Smartphone {
  if (!transports || transports.length === 0) return Key;
  if (transports.includes('internal')) return Smartphone;
  if (transports.includes('usb')) return Usb;
  if (transports.includes('nfc') || transports.includes('ble')) return Wifi;
  if (transports.includes('hybrid')) return Globe;
  return Key;
 }

 function formatDate(s?: string): string {
  if (!s) return '';
  const d = new Date(s);
  if (isNaN(d.getTime())) return '';
  return d.toLocaleString(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
 }

 function timeAgo(s?: string): string {
  if (!s) return 'never';
  const d = new Date(s);
  if (isNaN(d.getTime())) return '';
  const diff = (Date.now() - d.getTime()) / 1000;
  if (diff < 60) return 'just now';
  if (diff < 3600) return `${Math.floor(diff / 60)} min ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} h ago`;
  if (diff < 604800) return `${Math.floor(diff / 86400)} d ago`;
  return formatDate(s);
 }
</script>

<PanelHeader title="Account security">
 How you, <span class="font-semibold">{appStore.info?.user || appStore.identity?.subject}</span>, sign in to kutu.
</PanelHeader>

<div class="flex flex-col gap-10">
 {#if isLocalAccount}
  <section>
   <PanelHeader title="Password" level={2}>Changing it keeps you signed in here. Access tokens are not affected.</PanelHeader>
   <form class="leaf p-4" onsubmit={changePassword}>
    <div class="grid grid-cols-1 md:grid-cols-3 gap-4">
     <label class="field">
      <span class="field-label">Current password</span>
      <input type={showPasswords ? 'text' : 'password'} class="input font-mono" bind:value={currentPassword} autocomplete="current-password" oninput={() => (passwordError = '')} />
     </label>
     <label class="field">
      <span class="field-label">New password</span>
      <input type={showPasswords ? 'text' : 'password'} class="input font-mono" bind:value={newPassword} autocomplete="new-password" />
     </label>
     <label class="field">
      <span class="field-label">Repeat new password</span>
      <input type={showPasswords ? 'text' : 'password'} class="input font-mono" bind:value={confirmPassword} autocomplete="new-password" />
     </label>
    </div>
    <div class="mt-4 flex flex-wrap items-center justify-end gap-3">
     <button type="button" class="btn btn-ghost btn-sm mr-auto" onclick={() => (showPasswords = !showPasswords)}>
      {#if showPasswords}<EyeOff size={13} /> Hide{:else}<Eye size={13} /> Show{/if} passwords
     </button>
     {#if passwordError || passwordProblem}
      <span class="text-[13px] text-vermilion-700 dark:text-vermilion-300" role="alert">{passwordError || passwordProblem}</span>
     {/if}
     <button type="submit" class="btn btn-primary" disabled={passwordBusy || !currentPassword || !newPassword || newPassword !== confirmPassword || passwordProblem !== null}>
      {passwordBusy ? 'Changing…' : 'Change password'}
     </button>
    </div>
   </form>
  </section>
 {/if}

 <section>
  <PanelHeader title="Two-factor authentication" level={2}>
   A 6-digit code from an authenticator app (1Password, Bitwarden, Google Authenticator, …) after your password.
   {#snippet actions()}
    {#if totpStatus?.enabled}
     <span class="status status-ok">On</span>
    {:else if totpStatus?.pending_enrollment}
     <span class="status status-warn">Pending</span>
    {:else if totpStatus}
     <span class="status status-off">Off</span>
    {/if}
   {/snippet}
  </PanelHeader>

  {#if lastRecoveryCodes.length > 0}
   <div class="leaf mb-3 p-4 border-l-[3px] !border-l-[var(--sec-500)]" role="status">
    <div class="text-[14px] font-semibold text-slate-900 dark:text-warm-50">Save your recovery codes</div>
    <p class="mt-1 text-[13px] text-slate-600 dark:text-warm-300 max-w-[65ch]">Each code signs you in once if you lose your authenticator. kutu won't show them again.</p>
    <div class="mt-3 p-3 rounded-[3px] border border-slate-200 dark:border-warm-700 bg-slate-50 dark:bg-warm-900 font-mono text-[13px] grid grid-cols-2 sm:grid-cols-3 gap-y-1 gap-x-4 select-all">
     {#each lastRecoveryCodes as code}<span>{code}</span>{/each}
    </div>
    <div class="mt-3 flex justify-end gap-2">
     <button type="button" class="btn btn-secondary" onclick={() => copyToClipboard(lastRecoveryCodes.join('\n'), 'Recovery codes')}><Copy size={14} /> Copy all</button>
     <button type="button" class="btn btn-primary" onclick={() => (lastRecoveryCodes = [])}>I've saved them</button>
    </div>
   </div>
  {/if}

  <div class="leaf p-4">
   {#if totpLoading}
    <div class="h-10 animate-pulse" aria-busy="true" aria-label="Loading two-factor status"></div>
   {:else if totpLoadError}
    <p class="text-[13px] text-slate-600 dark:text-warm-300">{totpLoadError}</p>
   {:else if enrollment}
    <div class="flex flex-col sm:flex-row gap-5 items-start">
     {#if qrDataURI}
      <img src={qrDataURI} alt="QR code for your authenticator app" class="w-[216px] h-[216px] shrink-0 rounded-[3px] border border-slate-300 dark:border-warm-600 bg-white p-1.5" />
     {/if}
     <form class="flex-1 min-w-0 space-y-4" onsubmit={finishTOTPEnrollment}>
      <div>
       <div class="text-[14px] font-semibold">1. Scan the code with your authenticator app</div>
       <details class="mt-2 text-[13px]">
        <summary class="cursor-pointer text-slate-600 dark:text-warm-300 hover:text-slate-900 dark:hover:text-warm-50">Can't scan? Enter the secret by hand</summary>
        <div class="mt-2 flex items-center gap-2">
         <code class="input flex items-center font-mono break-all !h-auto py-1.5">{enrollment.secret_base32}</code>
         <button type="button" class="btn btn-secondary btn-icon shrink-0" onclick={() => copyToClipboard(enrollment!.secret_base32, 'Secret')} aria-label="Copy secret" title="Copy secret"><Copy size={14} /></button>
        </div>
        <p class="field-hint mt-1 font-mono">{enrollment.algorithm} · {enrollment.digits} digits · {enrollment.period}s</p>
       </details>
      </div>
      <div class="field">
       <label class="text-[14px] font-semibold" for="totp-enroll-code">2. Enter the 6-digit code it shows</label>
       <div class="flex flex-wrap gap-2">
        <input id="totp-enroll-code" type="text" inputmode="numeric" maxlength="6" autocomplete="one-time-code" bind:value={enrollmentCode} placeholder="123456" class="input !w-36 text-center font-mono !text-[16px] tracking-widest" />
        <button type="submit" class="btn btn-primary" disabled={enrollmentBusy}><Check size={14} /> Verify and turn on</button>
        <button type="button" class="btn btn-secondary" onclick={cancelEnrollment}>Cancel</button>
       </div>
      </div>
     </form>
    </div>
   {:else if totpStatus?.enabled}
    <div class="space-y-4">
     <p class="text-[13px] text-slate-600 dark:text-warm-300">
      On since {formatDate(totpStatus.created_at)} ·
      <span class={totpStatus.recovery_codes_left < 3 ? 'text-vermilion-700 dark:text-vermilion-300 font-semibold' : ''}>
       {totpStatus.recovery_codes_left} recovery {totpStatus.recovery_codes_left === 1 ? 'code' : 'codes'} left
      </span>
     </p>
     {#if totpStatus.recovery_codes_left === 0}
      <p class="status status-err">No recovery codes left. If you lose your authenticator you'll be locked out; generate new ones.</p>
     {:else if totpStatus.recovery_codes_left < 3}
      <p class="status status-warn">Running low on recovery codes. Generate a fresh set.</p>
     {/if}
     <div class="flex flex-wrap gap-2">
      <button type="button" class="btn btn-secondary" onclick={() => { showRegen = !showRegen; showDisable = false; }} aria-expanded={showRegen}><RefreshCw size={14} /> New recovery codes</button>
      <button type="button" class="btn btn-danger-ghost" onclick={() => { showDisable = !showDisable; showRegen = false; }} aria-expanded={showDisable}><Trash2 size={14} /> Turn off</button>
     </div>
     {#if showRegen}
      <form class="pt-4 border-t border-dashed border-slate-300 dark:border-warm-600" onsubmit={regenerateRecoveryCodes}>
       <label class="field max-w-md">
        <span class="field-label">Confirm with your password</span>
        <span class="field-hint">Every previous recovery code stops working.</span>
        <span class="flex gap-2">
         <input type="password" class="input font-mono" bind:value={regenPassword} autocomplete="current-password" />
         <button type="submit" class="btn btn-primary shrink-0" disabled={regenBusy || !regenPassword}>{regenBusy ? 'Generating…' : 'Generate'}</button>
        </span>
       </label>
      </form>
     {/if}
     {#if showDisable}
      <form class="pt-4 border-t border-dashed border-slate-300 dark:border-warm-600" onsubmit={disableTOTP}>
       <label class="field max-w-md">
        <span class="field-label">Confirm with your password</span>
        <span class="field-hint">Afterwards your password alone signs you in. Your passkeys keep working.</span>
        <span class="flex gap-2">
         <input type="password" class="input font-mono" bind:value={disablePassword} autocomplete="current-password" />
         <button type="submit" class="btn btn-danger shrink-0" disabled={disableBusy || !disablePassword}>{disableBusy ? 'Turning off…' : 'Turn off 2FA'}</button>
        </span>
       </label>
      </form>
     {/if}
    </div>
   {:else}
    <div class="flex flex-wrap items-center gap-3">
     <button type="button" class="btn btn-primary" disabled={enrollmentBusy} onclick={startTOTPEnrollment}><Plus size={14} /> Set up two-factor authentication</button>
     {#if totpStatus?.pending_enrollment}
      <span class="text-[13px] text-slate-600 dark:text-warm-300">A previous setup wasn't finished. Starting again replaces its QR code.</span>
     {/if}
    </div>
   {/if}
  </div>
 </section>

 <section>
  <PanelHeader title="Passkeys" level={2}>
   Sign in with Touch ID, Windows Hello, your phone or a hardware key instead of typing a password.
   {#snippet actions()}
    {#if !loading && !loadError}<span class="text-[12px] text-slate-500 dark:text-warm-400 font-mono">{passkeys.length} enrolled</span>{/if}
   {/snippet}
  </PanelHeader>

  <div class="leaf overflow-hidden">
   {#if !webauthnSupported}
    <p class="p-4 text-[13px] text-slate-600 dark:text-warm-300">This browser doesn't support passkeys. Try a recent Chrome, Edge, Firefox or Safari.</p>
   {:else if loadError}
    <p class="p-4 text-[13px] text-slate-600 dark:text-warm-300">{loadError}</p>
   {:else}
    <form class="p-4 border-b border-slate-200 dark:border-warm-700" onsubmit={handleEnroll}>
     <div class="flex flex-wrap items-end gap-3">
      <label class="field flex-1 min-w-48">
       <span class="field-label">Name <span class="font-normal text-slate-500 dark:text-warm-400">optional</span></span>
       <input type="text" class="input" bind:value={newName} placeholder="MacBook, YubiKey 5" maxlength="64" />
      </label>
      <label class="field">
       <span class="field-label">Device</span>
       <select class="input !w-auto" bind:value={newAttachment}>
        <option value="">Any (recommended)</option>
        <option value="platform">This device only</option>
        <option value="cross-platform">Security key</option>
       </select>
      </label>
      <button type="submit" class="btn btn-primary" disabled={enrolling}><Plus size={14} /> {enrolling ? 'Waiting for device…' : 'Add passkey'}</button>
     </div>
    </form>

    {#if loading}
     <div class="h-16 animate-pulse" aria-busy="true" aria-label="Loading passkeys"></div>
    {:else if passkeys.length === 0}
     <p class="px-6 py-8 text-center text-[13px] text-slate-600 dark:text-warm-300">No passkeys yet.</p>
    {:else}
     <ul class="divide-y divide-slate-200 dark:divide-warm-700">
      {#each passkeys as p (p.id)}
       {@const Icon = transportIcon(p.transports)}
       <li class="px-4 py-3 flex items-center gap-3 {renamingID === p.id ? 'bg-accent-50/50 dark:bg-accent-950/30' : ''}">
        <Icon size={17} class="shrink-0 text-slate-500 dark:text-warm-400" />
        <div class="flex-1 min-w-0">
         {#if renamingID === p.id}
          <div class="flex items-center gap-1.5">
           <!-- svelte-ignore a11y_autofocus -->
           <input
            type="text"
            class="input !h-7"
            bind:value={renameDraft}
            maxlength="64"
            autofocus
            aria-label="Passkey name"
            onkeydown={(e) => {
             if (e.key === 'Enter') { e.preventDefault(); saveRename(p); }
             else if (e.key === 'Escape') { e.preventDefault(); cancelRename(); }
            }}
           />
           <button type="button" class="btn btn-primary btn-sm btn-icon" onclick={() => saveRename(p)} aria-label="Save name"><Check size={14} /></button>
           <button type="button" class="btn btn-ghost btn-sm btn-icon" onclick={cancelRename} aria-label="Cancel rename"><X size={14} /></button>
          </div>
         {:else}
          <div class="text-[14px] font-semibold truncate">{p.name}</div>
          <div class="text-[12px] text-slate-500 dark:text-warm-400 flex flex-wrap items-center gap-x-2">
           <span>Added {formatDate(p.created_at)}</span>
           <span>Last used {timeAgo(p.last_used_at)}</span>
           {#if p.backup_state}<span class="tag" title="Synced across your devices (e.g. iCloud Keychain)">synced</span>{/if}
          </div>
         {/if}
        </div>
        {#if renamingID !== p.id}
         <span class="flex items-center gap-1 shrink-0">
          <button type="button" class="btn btn-ghost btn-sm" onclick={() => startRename(p)}><Pencil size={13} /> Rename</button>
          <button type="button" class="btn btn-danger-ghost btn-sm btn-icon" onclick={() => handleDelete(p)} aria-label="Delete passkey {p.name}" title="Delete passkey"><Trash2 size={14} /></button>
         </span>
        {/if}
       </li>
      {/each}
     </ul>
    {/if}
   {/if}
  </div>
 </section>
</div>
