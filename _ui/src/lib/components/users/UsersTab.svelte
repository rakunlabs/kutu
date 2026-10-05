<script lang="ts">
 import {
  Trash2,
  UserCheck,
  UserX,
  Pencil,
  LogOut,
  Search,
  ChevronUp,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  ShieldOff,
  Plus,
  Eye,
  EyeOff,
 } from 'lucide-svelte';
 import { apiServerMessage } from '@/lib/api/client';
 import { appStore, type UserInfo } from '@/lib/store/store.svelte';
 import { addToast } from '@/lib/store/toast.svelte';
 import { confirmAction } from '@/lib/store/confirm.svelte';
 import PanelHeader from '@/lib/components/settings/PanelHeader.svelte';
 import type { UserQueryState } from './userQuery.svelte';

 type Props = {
  query: UserQueryState;
  canManagePermissions: boolean;
  onEdit: (user: UserInfo) => void;
  reload: () => void;
 };

 let { query, canManagePermissions, onEdit, reload }: Props = $props();

 let showCreate = $state(false);
 let newUsername = $state('');
 let newPassword = $state('');
 let showNewPassword = $state(false);
 let creating = $state(false);

 let searchTimeout: ReturnType<typeof setTimeout> | null = null;

 const users = $derived(appStore.users);
 const total = $derived(appStore.usersTotal);
 const currentUser = $derived(appStore.info?.user);
 const allPermissions = $derived(appStore.permissions);
 const totalPages = $derived(Math.max(1, Math.ceil(total / query.pageSize)));
 const showingFrom = $derived(total === 0 ? 0 : (query.currentPage - 1) * query.pageSize + 1);
 const showingTo = $derived(Math.min(query.currentPage * query.pageSize, total));

 function handlePermissionFilterChange(value: string) {
  query.filterPermissionId = value;
  query.currentPage = 1;
  reload();
 }

 function handleSearch(value: string) {
  query.searchText = value;
  if (searchTimeout) clearTimeout(searchTimeout);
  searchTimeout = setTimeout(() => {
   query.currentPage = 1;
   reload();
  }, 300);
 }

 function handleSort(field: string) {
  if (query.sortField === field) {
   query.sortDir = query.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
   query.sortField = field;
   query.sortDir = 'asc';
  }
  query.currentPage = 1;
  reload();
 }

 function goToPage(page: number) {
  if (page < 1 || page > totalPages) return;
  query.currentPage = page;
  reload();
 }

 function handlePageSizeChange(size: number) {
  query.pageSize = size;
  query.currentPage = 1;
  reload();
 }

 async function handleCreate(e?: Event) {
  e?.preventDefault();
  if (!newUsername.trim() || !newPassword) return;
  creating = true;
  try {
   await appStore.createUser(newUsername.trim(), newPassword);
   addToast(`User "${newUsername.trim()}" created`, 'success');
   newUsername = '';
   newPassword = '';
   showCreate = false;
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not create the user'), 'alert');
  } finally {
   creating = false;
  }
 }

 async function handleToggleDisabled(user: UserInfo) {
  if (!user.disabled) {
   const ok = await confirmAction({
    title: `Disable "${user.username}"?`,
    message: 'They are signed out and cannot sign in or use their access tokens until re-enabled. Their account and permissions are kept.',
    confirmLabel: 'Disable user',
    danger: true,
   });
   if (!ok) return;
  }
  try {
   await appStore.updateUser(user.id, { disabled: !user.disabled });
   addToast(`User "${user.username}" ${user.disabled ? 'enabled' : 'disabled'}`, 'success');
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not update the user'), 'alert');
  }
 }

 async function handleDelete(user: UserInfo) {
  const ok = await confirmAction({
   title: `Delete user "${user.username}"?`,
   message: 'Their sessions, access tokens and permission assignments are removed. Files and artifacts they uploaded stay.',
   confirmLabel: 'Delete user',
   danger: true,
  });
  if (!ok) return;
  try {
   await appStore.deleteUser(user.id);
   addToast('User deleted', 'success');
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not delete the user'), 'alert');
  }
 }

 async function handleKick(user: UserInfo) {
  const ok = await confirmAction({
   title: `Sign out "${user.username}" everywhere?`,
   message: 'All of their browser sessions end now. They can sign in again; access tokens keep working.',
   confirmLabel: 'Sign out',
  });
  if (!ok) return;
  try {
   await appStore.kickUser(user.id);
   addToast(`All sessions for "${user.username}" ended`, 'success');
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not end the sessions'), 'alert');
  }
 }

 // For users who lost both their authenticator and their recovery codes.
 async function handleResetTOTP(user: UserInfo) {
  const ok = await confirmAction({
   title: `Reset two-factor for "${user.username}"?`,
   message: 'Their authenticator enrollment and recovery codes are wiped. They sign in with the password alone until they enroll again.',
   confirmLabel: 'Reset 2FA',
   danger: true,
  });
  if (!ok) return;
  try {
   await appStore.resetUserTOTP(user.id);
   addToast(`2FA reset for "${user.username}"`, 'success');
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not reset 2FA'), 'alert');
  }
 }
</script>

{#snippet sortHead(field: string, label: string)}
 <button type="button" class="label-caps inline-flex items-center gap-1 text-[11px] hover:text-slate-900 dark:hover:text-warm-50 cursor-pointer" onclick={() => handleSort(field)}>
  {label}
  {#if query.sortField === field}
   {#if query.sortDir === 'asc'}<ChevronUp size={12} />{:else}<ChevronDown size={12} />{/if}
  {/if}
 </button>
{/snippet}

<section>
 <PanelHeader title="Users" level={2}>
  People who can sign in to kutu. Capabilities come from the permissions assigned to them.
  {#snippet actions()}
   <button type="button" class="btn btn-secondary btn-sm" onclick={() => (showCreate = true)} disabled={showCreate}><Plus size={13} /> Add user</button>
  {/snippet}
 </PanelHeader>

 <div class="flex flex-wrap items-center gap-2 mb-3">
  <div class="relative flex-1 min-w-48">
   <Search size={14} class="absolute left-2.5 top-1/2 -translate-y-1/2 text-slate-400 dark:text-warm-400 pointer-events-none" />
   <input type="search" value={query.searchText} oninput={(e) => handleSearch(e.currentTarget.value)} aria-label="Search users" class="input !pl-8" placeholder="Search by username" />
  </div>
  {#if canManagePermissions && allPermissions.length > 0}
   <select value={query.filterPermissionId} onchange={(e) => handlePermissionFilterChange(e.currentTarget.value)} aria-label="Filter users by permission" class="input !w-auto min-w-44">
    <option value="">Any permission</option>
    {#each allPermissions as p (p.id)}
     <option value={p.id}>{p.name}</option>
    {/each}
   </select>
  {/if}
 </div>

 <div class="leaf overflow-hidden">
  {#if showCreate}
   <form class="p-4 border-b border-slate-200 dark:border-warm-700 bg-accent-50/50 dark:bg-accent-950/30" onsubmit={handleCreate}>
    <div class="text-[13px] font-semibold text-slate-900 dark:text-warm-50 mb-3">New user</div>
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
     <label class="field">
      <span class="field-label">Username</span>
      <input type="text" class="input font-mono" bind:value={newUsername} autocomplete="off" placeholder="alice" />
     </label>
     <label class="field">
      <span class="field-label">Password</span>
      <span class="relative flex">
       <input type={showNewPassword ? 'text' : 'password'} class="input font-mono pr-9" bind:value={newPassword} autocomplete="new-password" />
       <button type="button" class="btn btn-ghost btn-sm btn-icon absolute right-0.5 top-1/2 -translate-y-1/2" aria-label={showNewPassword ? 'Hide password' : 'Show password'} onclick={() => (showNewPassword = !showNewPassword)}>
        {#if showNewPassword}<EyeOff size={14} />{:else}<Eye size={14} />{/if}
       </button>
      </span>
     </label>
    </div>
    <p class="field-hint mt-3">New users have no capabilities until you assign permissions to them.</p>
    <div class="mt-4 flex items-center justify-end gap-2">
     <button type="button" class="btn btn-secondary" onclick={() => (showCreate = false)}>Cancel</button>
     <button type="submit" class="btn btn-primary" disabled={creating || !newUsername.trim() || !newPassword}>{creating ? 'Creating…' : 'Create user'}</button>
    </div>
   </form>
  {/if}

  <div class="hidden md:flex items-center gap-4 px-4 py-2 border-b border-slate-200 dark:border-warm-700 text-slate-500 dark:text-warm-400">
   <span class="w-64">{@render sortHead('username', 'Username')}</span>
   <span class="w-24 label-caps text-[11px]">Status</span>
   <span class="w-20 label-caps text-[11px]">Sessions</span>
   <span class="w-28">{@render sortHead('created_at', 'Created')}</span>
  </div>

  {#if users.length === 0}
   <p class="px-6 py-8 text-center text-[13px] text-slate-600 dark:text-warm-300">
    {query.searchText || query.filterPermissionId ? 'No users match these filters.' : 'No users found.'}
   </p>
  {:else}
   <ul class="divide-y divide-slate-200 dark:divide-warm-700">
    {#each users as user (user.id)}
     {@const isYou = user.username === currentUser}
     {@const isOnline = user.active_sessions > 0}
     <li class="sel {isYou ? 'is-sel' : ''} flex flex-wrap md:flex-nowrap items-center gap-x-4 gap-y-1.5 px-4 py-2.5">
      <div class="md:w-64 min-w-0 flex items-center gap-1.5 flex-wrap">
       <button type="button" class="font-mono text-[14px] font-semibold text-left text-slate-900 dark:text-warm-50 hover:text-accent-700 dark:hover:text-accent-300 cursor-pointer truncate max-w-40" onclick={() => onEdit(user)}>{user.username}</button>
       {#if isYou}<span class="tag">you</span>{/if}
       {#if user.is_superadmin}<span class="tag">superadmin</span>{/if}
       {#if user.external}<span class="tag" title="Signs in through an identity provider">external</span>{/if}
       {#if user.has_totp}<span class="tag" title="Two-factor authentication is on">2FA</span>{/if}
      </div>
      <span class="md:w-24">
       {#if user.disabled}<span class="status status-err">Disabled</span>{:else}<span class="status status-ok">Active</span>{/if}
      </span>
      <span class="md:w-20">
       <span class="status {isOnline ? 'status-ok' : 'status-off'}"><span class="font-mono">{user.active_sessions}</span><span class="md:sr-only">{user.active_sessions === 1 ? 'session' : 'sessions'}</span></span>
      </span>
      <span class="md:w-28 font-mono text-[12px] text-slate-500 dark:text-warm-400">{new Date(user.created_at).toLocaleDateString()}</span>
      <span class="flex items-center gap-1 ml-auto">
       <button type="button" class="btn btn-ghost btn-sm" onclick={() => onEdit(user)}><Pencil size={13} /> Edit</button>
       {#if !isYou && isOnline}
        <button type="button" class="btn btn-ghost btn-sm btn-icon" onclick={() => handleKick(user)} title="Sign out everywhere" aria-label="Sign out {user.username} everywhere"><LogOut size={14} /></button>
       {/if}
       {#if !isYou}
        <button type="button" class="btn btn-ghost btn-sm btn-icon" onclick={() => handleToggleDisabled(user)} title={user.disabled ? 'Enable user' : 'Disable user'} aria-label={user.disabled ? `Enable user ${user.username}` : `Disable user ${user.username}`}>
         {#if user.disabled}<UserCheck size={14} />{:else}<UserX size={14} />{/if}
        </button>
       {/if}
       {#if user.has_totp && !isYou}
        <button type="button" class="btn btn-danger-ghost btn-sm btn-icon" onclick={() => handleResetTOTP(user)} title="Reset 2FA" aria-label="Reset 2FA for {user.username}"><ShieldOff size={14} /></button>
       {/if}
       {#if !isYou}
        <button type="button" class="btn btn-danger-ghost btn-sm btn-icon" onclick={() => handleDelete(user)} title="Delete user" aria-label="Delete user {user.username}"><Trash2 size={14} /></button>
       {/if}
      </span>
     </li>
    {/each}
   </ul>
  {/if}

  {#if total > 0}
   <div class="flex flex-wrap items-center justify-between gap-2 px-4 py-2.5 border-t border-slate-200 dark:border-warm-700 text-[12px] text-slate-500 dark:text-warm-400">
    <div class="flex items-center gap-3">
     <span class="font-mono">{showingFrom}–{showingTo} of {total}</span>
     <label class="flex items-center gap-1.5">
      <span class="sr-only">Rows per page</span>
      <select value={query.pageSize} onchange={(e) => handlePageSizeChange(Number(e.currentTarget.value))} class="input !h-7 !w-auto !text-[12px]">
       <option value={10}>10 / page</option>
       <option value={20}>20 / page</option>
       <option value={50}>50 / page</option>
       <option value={100}>100 / page</option>
      </select>
     </label>
    </div>
    <div class="flex items-center gap-1">
     <button type="button" class="btn btn-ghost btn-sm btn-icon" onclick={() => goToPage(query.currentPage - 1)} disabled={query.currentPage <= 1} aria-label="Previous page"><ChevronLeft size={15} /></button>
     <span class="font-mono px-1">{query.currentPage} / {totalPages}</span>
     <button type="button" class="btn btn-ghost btn-sm btn-icon" onclick={() => goToPage(query.currentPage + 1)} disabled={query.currentPage >= totalPages} aria-label="Next page"><ChevronRight size={15} /></button>
    </div>
   </div>
  {/if}
 </div>
</section>
