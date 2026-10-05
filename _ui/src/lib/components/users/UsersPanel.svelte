<script lang="ts">
 // Settings → Users: the user list and, for permissions.manage holders,
 // the permission bundles. Port of pika's Users page.
 import { appStore, type UserInfo, type PermissionInfo } from '@/lib/store/store.svelte';
 import PanelHeader from '@/lib/components/settings/PanelHeader.svelte';
 import UsersTab from './UsersTab.svelte';
 import PermissionsTab from './PermissionsTab.svelte';
 import EditUserModal from './EditUserModal.svelte';
 import EditPermissionModal from './EditPermissionModal.svelte';
 import { UserQueryState, type KnownCapability } from './userQuery.svelte';

 const canManageUsers = $derived(appStore.hasPermission('users.manage'));
 const canManagePermissions = $derived(appStore.hasPermission('permissions.manage'));

 let picked = $state<'users' | 'permissions' | null>(null);
 const activeTab = $derived.by<'users' | 'permissions' | null>(() => {
  if (picked === 'users' && canManageUsers) return 'users';
  if (picked === 'permissions' && canManagePermissions) return 'permissions';
  return canManageUsers ? 'users' : canManagePermissions ? 'permissions' : null;
 });

 const query = new UserQueryState();
 let editingUser = $state<UserInfo | null>(null);
 let editingPerm = $state<PermissionInfo | null>(null);

 const knownKeys = $derived<KnownCapability[]>(appStore.info?.capabilities ?? []);

 function viewUsersWithPermission(permissionId: string) {
  query.filterPermissionId = permissionId;
  query.searchText = '';
  query.currentPage = 1;
  picked = 'users';
  reload();
 }

 function reload() {
  appStore.loadUsers(query.build());
 }

 // Load each resource the first time its capability is present.
 let usersLoaded = false;
 let permissionsLoaded = false;
 $effect(() => {
  if (canManageUsers && !usersLoaded) {
   usersLoaded = true;
   reload();
  }
  if (canManagePermissions && !permissionsLoaded) {
   permissionsLoaded = true;
   appStore.loadPermissions();
  }
 });
</script>

<PanelHeader title="Users & permissions">
 Who can sign in, and what each person is allowed to read, publish or administer.
</PanelHeader>

{#if canManageUsers && canManagePermissions}
 <div class="flex gap-1 mb-6 border-b border-slate-300 dark:border-warm-700" role="tablist" aria-label="Users and permissions">
  <button type="button" role="tab" aria-selected={activeTab === 'users'} class="ptab label-caps {activeTab === 'users' ? 'is-on' : ''}" onclick={() => (picked = 'users')}>Users</button>
  <button type="button" role="tab" aria-selected={activeTab === 'permissions'} class="ptab label-caps {activeTab === 'permissions' ? 'is-on' : ''}" onclick={() => (picked = 'permissions')}>Permissions</button>
 </div>
{/if}

{#if activeTab === 'users'}
 <UsersTab {query} {canManagePermissions} onEdit={(u) => (editingUser = u)} {reload} />
{:else if activeTab === 'permissions'}
 <PermissionsTab {knownKeys} {canManageUsers} onEdit={(p) => (editingPerm = p)} onViewUsers={viewUsersWithPermission} />
{/if}

{#if editingUser}
 {#key editingUser.id}
  <EditUserModal user={editingUser} {knownKeys} {canManagePermissions} onClose={() => (editingUser = null)} />
 {/key}
{/if}

{#if editingPerm}
 {#key editingPerm.id}
  <EditPermissionModal perm={editingPerm} {knownKeys} onClose={() => (editingPerm = null)} />
 {/key}
{/if}

<style>
 .ptab {
  height: 2.25rem;
  padding: 0 0.75rem;
  margin-bottom: -1px;
  font-size: 12px;
  color: var(--color-slate-500);
  border-bottom: 2px solid transparent;
  cursor: pointer;
 }

 .ptab:hover {
  color: var(--color-slate-900);
 }

 .ptab.is-on {
  color: var(--sec-700);
  border-bottom-color: var(--sec-500);
 }

 :global(.dark) .ptab {
  color: var(--color-warm-400);
 }

 :global(.dark) .ptab:hover {
  color: var(--color-warm-50);
 }

 :global(.dark) .ptab.is-on {
  color: var(--sec-300);
 }
</style>
