<script lang="ts">
 // Settings → Access tokens. Tokens authenticate CLI and registry
 // clients; each one is limited to the scopes listed on it. The raw key
 // is returned once on create and never again.
 import { onMount } from 'svelte';
 import { Plus, Trash2, Copy, Check, Eye, EyeOff } from 'lucide-svelte';
 import { apiServerMessage } from '@/lib/api/client';
 import { addToast } from '@/lib/store/toast.svelte';
 import { confirmAction } from '@/lib/store/confirm.svelte';
 import { tokenStore, type CreateTokenRequest, type TokenOperation, type TokenScope } from '@/lib/store/tokens.svelte';
 import PanelHeader from '@/lib/components/settings/PanelHeader.svelte';

 const OPS: TokenOperation[] = ['read', 'write', 'delete'];
 const PRESETS: { label: string; path: string }[] = [
  { label: 'Everything', path: '**' },
  { label: 'All raw mounts', path: 'raw/**' },
  { label: 'One raw mount', path: 'raw/builds/**' },
  { label: 'All registries', path: 'registry/**' },
  { label: 'One repository', path: 'registry/team-a/npm/**' },
 ];

 let showCreate = $state(false);
 let name = $state('');
 let scopes = $state<TokenScope[]>([{ path: '**', operations: ['read'] }]);
 let expiry = $state('');
 let creating = $state(false);

 let createdKey = $state<string | null>(null);
 let createdName = $state('');
 let showKey = $state(false);
 let copied = $state(false);

 const tokens = $derived(tokenStore.tokens);

 onMount(() => {
  void tokenStore.load();
 });

 function formatDate(s: string): string {
  return new Date(s).toLocaleString(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
 }

 function resetForm() {
  name = '';
  scopes = [{ path: '**', operations: ['read'] }];
  expiry = '';
 }

 function addScope() {
  scopes = [...scopes, { path: '', operations: ['read'] }];
 }

 function removeScope(i: number) {
  scopes = scopes.filter((_, idx) => idx !== i);
 }

 function hasOp(s: TokenScope, op: TokenOperation): boolean {
  return s.operations.includes('*') || s.operations.includes(op);
 }

 function toggleOp(i: number, op: TokenOperation) {
  const s = scopes[i];
  let ops = s.operations.includes('*') ? [...OPS] : [...s.operations];
  ops = ops.includes(op) ? ops.filter((o) => o !== op) : [...ops, op];
  s.operations = ops;
 }

 const problem = $derived.by(() => {
  if (!name.trim()) return 'Name is required.';
  if (scopes.length === 0) return 'Add at least one scope.';
  if (scopes.some((s) => !s.path.trim())) return 'Every scope needs a path.';
  if (scopes.some((s) => s.operations.length === 0)) return 'Every scope needs at least one operation.';
  if (scopes.some((s) => s.path.trim().startsWith('/'))) return 'Scope paths have no leading slash.';
  return null;
 });

 async function handleCreate(e?: Event) {
  e?.preventDefault();
  if (problem) return;
  creating = true;
  try {
   const req: CreateTokenRequest = {
    name: name.trim(),
    scopes: scopes.map((s) => ({ path: s.path.trim(), operations: [...s.operations] })),
   };
   if (expiry) req.expires_at = new Date(expiry).toISOString();
   const result = await tokenStore.create(req);
   createdKey = result.raw_key;
   createdName = result.name;
   showKey = false;
   copied = false;
   showCreate = false;
   resetForm();
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not create the token'), 'alert');
  } finally {
   creating = false;
  }
 }

 async function handleDelete(id: string, tokenName: string) {
  const ok = await confirmAction({
   title: `Delete token "${tokenName}"?`,
   message: 'Every client using it is refused from now on. This cannot be undone; create a new token to restore access.',
   confirmLabel: 'Delete token',
   danger: true,
  });
  if (!ok) return;
  try {
   await tokenStore.remove(id);
   addToast('Token deleted', 'success');
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not delete the token'), 'alert');
  }
 }

 async function handleToggle(id: string, active: boolean) {
  try {
   await tokenStore.patch(id, { active: !active });
   addToast(active ? 'Token disabled' : 'Token enabled', 'success');
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not update the token'), 'alert');
  }
 }

 async function copyKey() {
  if (!createdKey) return;
  try {
   await navigator.clipboard.writeText(createdKey);
   copied = true;
   addToast('Token copied to the clipboard', 'success');
  } catch {
   addToast('Could not copy to the clipboard', 'alert');
  }
 }

 function opsLabel(ops: string[]): string {
  if (ops.includes('*') || OPS.every((o) => ops.includes(o))) return 'all';
  return ops.join(', ');
 }

 function expired(t: { expires_at?: string }): boolean {
  return !!t.expires_at && new Date(t.expires_at).getTime() < Date.now();
 }
</script>

<PanelHeader title="Access tokens">
 Credentials for scripts, CI and package managers. Each token can only reach the paths in its scopes.
 {#snippet actions()}
  <button type="button" class="btn btn-primary" onclick={() => (showCreate = true)} disabled={showCreate}><Plus size={14} /> New token</button>
 {/snippet}
</PanelHeader>

{#if createdKey}
 <div class="leaf mb-6 p-4 border-l-[3px] !border-l-[var(--sec-500)]" role="status">
  <div class="text-[14px] font-semibold text-slate-900 dark:text-warm-50">Token "{createdName}" created</div>
  <p class="mt-1 text-[13px] text-slate-600 dark:text-warm-300">Copy it now. kutu stores only a hash and cannot show it again.</p>
  <div class="mt-3 flex items-center gap-2">
   <code class="input flex items-center font-mono overflow-hidden text-ellipsis whitespace-nowrap select-all">{showKey ? createdKey : '•'.repeat(Math.min(createdKey.length, 40))}</code>
   <button type="button" class="btn btn-secondary btn-icon shrink-0" onclick={() => (showKey = !showKey)} aria-label={showKey ? 'Hide token' : 'Show token'} title={showKey ? 'Hide' : 'Show'}>
    {#if showKey}<EyeOff size={14} />{:else}<Eye size={14} />{/if}
   </button>
   <button type="button" class="btn btn-secondary shrink-0" onclick={copyKey}>
    {#if copied}<Check size={14} />{:else}<Copy size={14} />{/if} Copy
   </button>
  </div>
  <div class="mt-3 flex justify-end">
   <button type="button" class="btn btn-ghost btn-sm" onclick={() => (createdKey = null)}>I've saved it</button>
  </div>
 </div>
{/if}

{#if showCreate}
 <form class="leaf mb-6 p-4 bg-accent-50/50 dark:bg-accent-950/30" onsubmit={handleCreate}>
  <div class="text-[13px] font-semibold text-slate-900 dark:text-warm-50 mb-3">New token</div>
  <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
   <label class="field">
    <span class="field-label">Name</span>
    <input type="text" class="input" bind:value={name} placeholder="ci-publisher" autocomplete="off" />
   </label>
   <label class="field">
    <span class="field-label">Expires <span class="font-normal text-slate-500 dark:text-warm-400">optional</span></span>
    <input type="datetime-local" class="input" bind:value={expiry} />
    <span class="field-hint">Leave empty for a token that never expires.</span>
   </label>
  </div>

  <div class="mt-4 pt-4 border-t border-dashed border-slate-300 dark:border-warm-600">
   <div class="flex items-center justify-between gap-3">
    <span class="field-label">Scopes</span>
    <button type="button" class="btn btn-ghost btn-sm" onclick={addScope}><Plus size={12} /> Add scope</button>
   </div>
   <p class="field-hint mt-1 max-w-[70ch]">
    Paths look like <code class="font-mono">raw/&lt;mount&gt;/&lt;glob&gt;</code> or <code class="font-mono">registry/&lt;namespace&gt;/&lt;repo&gt;/**</code>;
    <code class="font-mono">**</code> alone covers everything. <code class="font-mono">*</code> matches one segment, <code class="font-mono">**</code> any number.
   </p>
   <div class="mt-2 space-y-2">
    {#each scopes as scope, i (i)}
     <div class="rounded-[3px] border border-slate-300 dark:border-warm-600 bg-white dark:bg-warm-900 p-3">
      <div class="flex items-start gap-2">
       <label class="field flex-1">
        <span class="sr-only">Path pattern for scope {i + 1}</span>
        <input type="text" class="input font-mono" bind:value={scope.path} placeholder="registry/team-a/npm/**" list="token-path-presets" />
       </label>
       {#if scopes.length > 1}
        <button type="button" class="btn btn-danger-ghost btn-icon" onclick={() => removeScope(i)} aria-label="Remove scope {i + 1}" title="Remove scope"><Trash2 size={14} /></button>
       {/if}
      </div>
      <div class="mt-2 flex flex-wrap items-center gap-x-4 gap-y-2">
       {#each OPS as op}
        <label class="check"><input type="checkbox" checked={hasOp(scope, op)} onchange={() => toggleOp(i, op)} /> {op}</label>
       {/each}
       <span class="flex flex-wrap gap-1 ml-auto">
        {#each PRESETS as p}
         <button type="button" class="btn btn-ghost btn-sm !h-6 !text-[12px]" onclick={() => (scope.path = p.path)} title={p.path}>{p.label}</button>
        {/each}
       </span>
      </div>
     </div>
    {/each}
   </div>
   <datalist id="token-path-presets">
    {#each PRESETS as p}<option value={p.path}>{p.label}</option>{/each}
   </datalist>
  </div>

  <div class="mt-5 flex flex-wrap items-center justify-end gap-2">
   <span class="mr-auto text-[13px] text-slate-500 dark:text-warm-400">{name ? (problem ?? '') : ''}</span>
   <button type="button" class="btn btn-secondary" onclick={() => { showCreate = false; resetForm(); }}>Cancel</button>
   <button type="submit" class="btn btn-primary" disabled={creating || problem !== null}>{creating ? 'Creating…' : 'Create token'}</button>
  </div>
 </form>
{/if}

<div class="leaf overflow-hidden">
 {#if !tokenStore.loaded}
  <div class="h-24 animate-pulse" aria-busy="true" aria-label="Loading tokens"></div>
 {:else if tokens.length === 0}
  <p class="px-6 py-8 text-center text-[13px] text-slate-600 dark:text-warm-300">No access tokens yet. Create one to let a script or package manager in.</p>
 {:else}
  <ul class="divide-y divide-slate-200 dark:divide-warm-700">
   {#each tokens as token (token.id)}
    <li class="flex flex-wrap md:flex-nowrap items-start gap-x-4 gap-y-2 px-4 py-3">
     <div class="flex-1 min-w-0">
      <div class="flex items-center gap-2 flex-wrap">
       <span class="text-[14px] font-semibold text-slate-900 dark:text-warm-50">{token.name}</span>
       {#if !token.active}
        <span class="status status-off">Disabled</span>
       {:else if expired(token)}
        <span class="status status-err">Expired</span>
       {:else}
        <span class="status status-ok">Active</span>
       {/if}
      </div>
      <div class="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-[12px] text-slate-500 dark:text-warm-400">
       <span>Created {formatDate(token.created_at)}{#if token.created_by} by <span class="text-slate-700 dark:text-warm-200">{token.created_by}</span>{/if}</span>
       {#if token.expires_at}<span>Expires {formatDate(token.expires_at)}</span>{/if}
       <span>Last used {token.last_used_at ? formatDate(token.last_used_at) : 'never'}</span>
      </div>
      <div class="mt-2 flex flex-wrap gap-1.5">
       {#each token.scopes as scope}
        <span class="tag !normal-case !tracking-normal font-mono">{scope.path} · {opsLabel(scope.operations)}</span>
       {/each}
      </div>
     </div>
     <span class="flex items-center gap-1 shrink-0">
      <button type="button" class="btn btn-ghost btn-sm" onclick={() => handleToggle(token.id, token.active)}>{token.active ? 'Disable' : 'Enable'}</button>
      <button type="button" class="btn btn-danger-ghost btn-sm btn-icon" onclick={() => handleDelete(token.id, token.name)} title="Delete token" aria-label="Delete token {token.name}"><Trash2 size={14} /></button>
     </span>
    </li>
   {/each}
  </ul>
 {/if}
</div>

<section class="mt-10">
 <PanelHeader title="Using a token" level={2}>
  Registries accept a token as <code class="font-mono">Authorization: Bearer &lt;token&gt;</code>, or as the password in Basic auth with any username.
 </PanelHeader>
 <dl class="leaf divide-y divide-slate-200 dark:divide-warm-700 text-[13px]">
  <div class="usage"><dt class="label-caps">Docker / OCI</dt><dd><code>docker login &lt;host&gt; -u token -p kutu_…</code></dd></div>
  <div class="usage"><dt class="label-caps">npm</dt><dd><code>//&lt;host&gt;/&lt;path&gt;/:_authToken=kutu_…</code> in <code>.npmrc</code></dd></div>
  <div class="usage"><dt class="label-caps">pip · Maven · Cargo</dt><dd>Basic auth: any username, the token as the password</dd></div>
  <div class="usage"><dt class="label-caps">curl</dt><dd><code>curl -H "Authorization: Bearer kutu_…" …</code></dd></div>
 </dl>
</section>

<style>
 .usage {
  display: grid;
  grid-template-columns: 10rem 1fr;
  gap: 0.75rem;
  align-items: baseline;
  padding: 0.625rem 1rem;
 }

 .usage dt {
  font-size: 11px;
  color: var(--color-slate-500);
 }

 .usage code {
  font-family: var(--font-mono);
  word-break: break-all;
 }

 :global(.dark) .usage dt {
  color: var(--color-warm-400);
 }

 @media (max-width: 639px) {
  .usage {
   grid-template-columns: 1fr;
   gap: 0.25rem;
  }
 }
</style>
