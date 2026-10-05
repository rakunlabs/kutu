<!--
 EditUserModal — details / permissions / access tabs for a single user.
 Mounted only while a user is being edited; all per-edit state lives
 here and is reset by remounting.
-->
<script lang="ts">
 import { untrack } from 'svelte';
 import { Ban, Link as LinkIcon, LogOut, Monitor, X } from 'lucide-svelte';
 import Modal from '@/lib/components/Modal.svelte';
 import { apiServerMessage } from '@/lib/api/client';
 import {
  appStore,
  type CapSource,
  type EffectiveReport,
  type PermissionInfo,
  type SessionView,
  type UserIdentity,
  type UserInfo,
 } from '@/lib/store/store.svelte';
 import { addToast } from '@/lib/store/toast.svelte';
 import type { KnownCapability } from './userQuery.svelte';

 type Props = {
  user: UserInfo;
  knownKeys: KnownCapability[];
  canManagePermissions: boolean;
  onClose: () => void;
 };

 let { user, knownKeys, canManagePermissions, onClose }: Props = $props();

 const allPermissions = $derived(appStore.permissions);

 let editPassword = $state('');
 let editUsername = $state(untrack(() => user.username));
 let editTab = $state<'details' | 'permissions' | 'access'>('details');
 let editUserPermissionIds = $state<string[]>([]);
 let loadingUserPerms = $state(true);
 let saving = $state(false);

 let effectiveReport = $state<EffectiveReport | null>(null);
 let userSessions = $state<SessionView[]>([]);
 let userIdentities = $state<UserIdentity[]>([]);
 let editDeniedCaps = $state<string[]>(untrack(() => user.denied_capabilities ?? []));
 let loadingAccess = $state(false);
 let accessLoaded = $state(false);

 $effect(() => {
  const id = untrack(() => user.id);
  if (!canManagePermissions) {
   loadingUserPerms = false;
   return;
  }
  appStore
   .getUserPermissions(id)
   .then((perms) => {
    editUserPermissionIds = perms.map((p: PermissionInfo) => p.id);
   })
   .catch(() => {
    editUserPermissionIds = [];
   })
   .finally(() => {
    loadingUserPerms = false;
   });
 });

 async function openAccessTab() {
  editTab = 'access';
  if (accessLoaded || loadingAccess) return;
  loadingAccess = true;
  try {
   const [rep, sess, idents] = await Promise.all([
    appStore.getUserEffectivePermissions(user.id),
    appStore.listUserSessions(user.id),
    appStore.getUserIdentities(user.id),
   ]);
   effectiveReport = rep;
   userSessions = sess;
   userIdentities = idents;
   editDeniedCaps = rep.denied ?? [];
   accessLoaded = true;
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not load access information'), 'alert');
  } finally {
   loadingAccess = false;
  }
 }

 // Deny overlay: persists immediately, separate from the modal Save.
 async function toggleDeny(capKey: string) {
  const next = editDeniedCaps.includes(capKey) ? editDeniedCaps.filter((k) => k !== capKey) : [...editDeniedCaps, capKey];
  try {
   await appStore.setUserDeniedPermissions(user.id, next);
   effectiveReport = await appStore.getUserEffectivePermissions(user.id);
   editDeniedCaps = effectiveReport.denied ?? next;
   addToast("Deny list updated. It applies on the user's next request.", 'success');
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not update the deny list'), 'alert');
  }
 }

 async function revokeSession(handle: string) {
  try {
   await appStore.revokeUserSession(user.id, handle);
   userSessions = userSessions.filter((s) => s.handle !== handle);
   addToast('Session revoked', 'success');
   await appStore.loadUsers();
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not revoke the session'), 'alert');
  }
 }

 function sourceLabel(s: CapSource): string {
  switch (s.kind) {
   case 'superadmin':
    return 'superadmin';
   case 'db_bundle':
    return `permission ${s.bundle}`;
   case 'role':
    return `role ${s.role}${s.bundle ? ` → ${s.bundle}` : ''}`;
   case 'scope':
    return `scope ${s.scope}${s.bundle ? ` → ${s.bundle}` : ''}`;
   default:
    return s.kind;
  }
 }

 function toggleEditPermission(permId: string) {
  editUserPermissionIds = editUserPermissionIds.includes(permId)
   ? editUserPermissionIds.filter((id) => id !== permId)
   : [...editUserPermissionIds, permId];
 }

 async function handleSave() {
  saving = true;
  try {
   const updates: { username?: string; password?: string } = {};
   if (editUsername.trim() && editUsername.trim() !== user.username) updates.username = editUsername.trim();
   if (editPassword) updates.password = editPassword;
   if (Object.keys(updates).length > 0) await appStore.updateUser(user.id, updates);
   if (canManagePermissions && !user.is_superadmin) await appStore.setUserPermissions(user.id, editUserPermissionIds);
   addToast('User updated', 'success');
   onClose();
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not update the user'), 'alert');
  } finally {
   saving = false;
  }
 }

 const tabs = $derived([
  { key: 'details' as const, label: 'Details' },
  ...(canManagePermissions ? [{ key: 'permissions' as const, label: 'Permissions' }] : []),
  { key: 'access' as const, label: 'Access' },
 ]);
</script>

<Modal open={true} {onClose} size="md" ariaLabel="Edit user {user.username}">
 {#snippet header()}
  <h2 class="flex items-center gap-2 min-w-0 text-[15px] font-semibold text-slate-900 dark:text-warm-50">
   <span class="truncate">Edit <span class="font-mono">{user.username}</span></span>
   {#if user.is_superadmin}<span class="tag">superadmin</span>{/if}
   {#if user.external}<span class="tag">external</span>{/if}
  </h2>
  <button type="button" class="btn btn-ghost btn-sm btn-icon" onclick={onClose} aria-label="Close"><X size={15} /></button>
 {/snippet}

 <div class="flex gap-1 px-4 border-b border-slate-200 dark:border-warm-700" role="tablist">
  {#each tabs as t (t.key)}
   <button
    type="button"
    role="tab"
    aria-selected={editTab === t.key}
    onclick={() => (t.key === 'access' ? openAccessTab() : (editTab = t.key))}
    class="mtab label-caps {editTab === t.key ? 'is-on' : ''}"
   >{t.label}</button>
  {/each}
 </div>

 <div class="p-4">
  {#if editTab === 'details'}
   <div class="space-y-4">
    <label class="field">
     <span class="field-label">Username</span>
     <input type="text" class="input font-mono" bind:value={editUsername} disabled={user.external} />
    </label>
    {#if !user.external}
     <label class="field">
      <span class="field-label">New password</span>
      <input type="password" class="input font-mono" bind:value={editPassword} autocomplete="new-password" placeholder="Leave empty to keep the current one" />
     </label>
    {:else}
     <p class="field-hint">This user signs in through an identity provider, so there is no local password to set.</p>
    {/if}
   </div>
  {:else if editTab === 'permissions'}
   {#if user.is_superadmin}
    <p class="text-[13px] text-slate-600 dark:text-warm-300">Superadmins hold every capability automatically.</p>
   {:else if loadingUserPerms}
    <p class="text-[13px] text-slate-500 dark:text-warm-400" aria-busy="true">Loading permissions…</p>
   {:else if allPermissions.length === 0}
    <p class="text-[13px] text-slate-600 dark:text-warm-300">No permissions exist yet. Create one on the Permissions tab first.</p>
   {:else}
    <ul class="leaf divide-y divide-slate-200 dark:divide-warm-700 max-h-72 overflow-y-auto">
     {#each allPermissions as perm (perm.id)}
      {@const checked = editUserPermissionIds.includes(perm.id)}
      <li>
       <label class="flex items-start gap-3 px-3 py-2 cursor-pointer hover:bg-slate-50 dark:hover:bg-warm-700/50">
        <input type="checkbox" class="mt-0.5" {checked} onchange={() => toggleEditPermission(perm.id)} />
        <span class="min-w-0">
         <span class="block text-[13px] font-semibold">{perm.name} <span class="font-mono font-normal text-[12px] text-slate-500 dark:text-warm-400">{perm.key}</span></span>
         <span class="block text-[12px] text-slate-500 dark:text-warm-400 font-mono truncate">{(perm.keys ?? []).join(', ')}</span>
        </span>
       </label>
      </li>
     {/each}
    </ul>
   {/if}
  {:else if loadingAccess}
   <p class="text-[13px] text-slate-500 dark:text-warm-400" aria-busy="true">Loading access…</p>
  {:else if effectiveReport}
   {@const rep = effectiveReport}
   <div class="space-y-5">
    <div class="flex flex-wrap items-center gap-2">
     <span class="status {rep.online ? 'status-ok' : 'status-off'}">{rep.online ? 'Online' : 'Offline'}</span>
     {#if rep.superadmin}<span class="tag">superadmin · {rep.superadmin_reason}</span>{/if}
     {#each rep.roles as role (role)}<span class="tag !normal-case font-mono">{role}</span>{/each}
    </div>
    {#if !rep.online}
     <p class="field-hint">Identity-provider roles only show while the user has an active session. Assigned permissions, superadmin and denies still apply.</p>
    {/if}

    <div>
     <h3 class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1.5">Effective capabilities</h3>
     <ul class="leaf divide-y divide-slate-200 dark:divide-warm-700">
      {#each knownKeys as cap (cap.key)}
       {@const granted = rep.capabilities.includes(cap.key)}
       {@const denied = editDeniedCaps.includes(cap.key)}
       {@const srcs = rep.sources.filter((s) => s.capability === cap.key)}
       {@const pats = rep.patterns?.[cap.key] ?? []}
       <li class="flex items-start gap-2 px-3 py-2">
        <div class="flex-1 min-w-0">
         <div class="flex items-center gap-2">
          <code class="font-mono text-[13px] {denied ? 'line-through text-slate-400 dark:text-warm-500' : ''}">{cap.key}</code>
          {#if denied}<span class="status status-err">Denied</span>{:else if granted}<span class="status status-ok">Granted</span>{:else}<span class="status status-off">Not granted</span>{/if}
         </div>
         {#if srcs.length > 0 || pats.length > 0}
          <div class="mt-0.5 flex flex-wrap gap-x-2 text-[12px] font-mono text-slate-500 dark:text-warm-400">
           {#each srcs as s, i (i)}<span>{sourceLabel(s)}</span>{/each}
           {#if pats.length > 0}<span title={pats.join('\n')}>paths: {pats.join(', ')}</span>{/if}
          </div>
         {/if}
        </div>
        {#if canManagePermissions && !(rep.superadmin && rep.superadmin_reason === 'allowlist')}
         <button
          type="button"
          onclick={() => toggleDeny(cap.key)}
          title={denied ? 'Allow this capability again' : 'Deny this capability for this user only'}
          aria-label={denied ? `Allow ${cap.key} again` : `Deny ${cap.key} for this user`}
          aria-pressed={denied}
          class="btn btn-sm btn-icon {denied ? 'btn-danger' : 'btn-danger-ghost'}"
         ><Ban size={13} /></button>
        {/if}
       </li>
      {/each}
     </ul>
     {#if rep.superadmin && rep.superadmin_reason === 'allowlist'}
      <p class="field-hint mt-1.5">This user is on the superadmin allowlist, so denies do not apply.</p>
     {/if}
    </div>

    {#if userIdentities.length > 0}
     <div>
      <h3 class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1.5">Linked accounts</h3>
      <ul class="leaf divide-y divide-slate-200 dark:divide-warm-700">
       {#each userIdentities as ident (ident.id)}
        <li class="flex items-center gap-2 px-3 py-2 text-[13px]">
         <LinkIcon size={13} class="shrink-0 text-slate-400" />
         <span class="font-semibold">{ident.provider}</span>
         <span class="font-mono text-[12px] text-slate-500 dark:text-warm-400 truncate">{ident.subject}</span>
        </li>
       {/each}
      </ul>
     </div>
    {/if}

    <div>
     <h3 class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1.5">Active sessions · {userSessions.length}</h3>
     {#if userSessions.length === 0}
      <p class="field-hint">No active sessions.</p>
     {:else}
      <ul class="leaf divide-y divide-slate-200 dark:divide-warm-700">
       {#each userSessions as sess (sess.handle)}
        <li class="flex items-center gap-2 px-3 py-2 text-[13px]">
         <Monitor size={13} class="shrink-0 text-slate-400" />
         <span class="font-semibold">{sess.provider || 'local'}</span>
         {#if sess.current}<span class="tag">this session</span>{/if}
         <span class="ml-auto font-mono text-[12px] text-slate-500 dark:text-warm-400">expires {new Date(sess.expires_at).toLocaleDateString()}</span>
         <button type="button" class="btn btn-danger-ghost btn-sm btn-icon" onclick={() => revokeSession(sess.handle)} title="Revoke this session" aria-label="Revoke this session"><LogOut size={13} /></button>
        </li>
       {/each}
      </ul>
     {/if}
    </div>
   </div>
  {:else}
   <p class="text-[13px] text-slate-500 dark:text-warm-400">No access information.</p>
  {/if}
 </div>

 {#snippet footer()}
  <button type="button" class="btn btn-secondary" onclick={onClose}>Cancel</button>
  <button type="button" class="btn btn-primary" onclick={handleSave} disabled={saving}>{saving ? 'Saving…' : 'Save changes'}</button>
 {/snippet}
</Modal>

<style>
 .mtab {
  height: 2.25rem;
  padding: 0 0.625rem;
  margin-bottom: -1px;
  font-size: 12px;
  color: var(--color-slate-500);
  border-bottom: 2px solid transparent;
  cursor: pointer;
 }

 .mtab:hover {
  color: var(--color-slate-900);
 }

 .mtab.is-on {
  color: var(--sec-700);
  border-bottom-color: var(--sec-500);
 }

 :global(.dark) .mtab {
  color: var(--color-warm-400);
 }

 :global(.dark) .mtab:hover {
  color: var(--color-warm-50);
 }

 :global(.dark) .mtab.is-on {
  color: var(--sec-300);
 }
</style>
