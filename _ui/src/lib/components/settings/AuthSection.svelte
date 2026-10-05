<script lang="ts">
 // Settings → Authentication. GET/PUT /api/v1/settings/auth: the whole
 // AuthSettings document is loaded, edited here and PUT back in full; the
 // server saves it and hot-reloads the auth stack.
 //
 // Fields this form doesn't edit (header strategy, UI theme, …) are kept
 // from the loaded document so a save never drops them. Durations are Go
 // time.Duration (nanoseconds) on the wire and seconds in the form.
 import { onMount } from 'svelte';
 import axios from 'axios';
 import { Plus, RotateCcw } from 'lucide-svelte';
 import { apiServerMessage } from '@/lib/api/client';
 import { addToast } from '@/lib/store/toast.svelte';
 import { appStore } from '@/lib/store/store.svelte';
 import PanelHeader from '@/lib/components/settings/PanelHeader.svelte';
 import Switch from '@/lib/components/settings/Switch.svelte';
 import OAuth2ProviderCard from '@/lib/components/auth/OAuth2ProviderCard.svelte';
 import PermissionMappingEditor from '@/lib/components/auth/PermissionMappingEditor.svelte';
 import StringListInput from '@/lib/components/auth/StringListInput.svelte';
 import {
  hasOAuth2ManualEndpoints,
  mapToRows,
  nsToSec,
  rowsToMap,
  secToNs,
  type AuthSettings,
  type MappingRow,
  type OAuth2Entry,
 } from '@/lib/components/auth/types';

 const availablePermissions = $derived(appStore.permissions ?? []);

 let loaded = $state(false);
 let loadError = $state('');
 let saving = $state(false);
 let base: AuthSettings = {};

 // Login screen
 let uiTitle = $state('');
 let uiSubtitle = $state('');

 // Registry
 let registryAnonymousRead = $state(false);

 // Local
 let localEnabled = $state(false);
 let localName = $state('');
 let localLoginFormCollapsed = $state(false);
 let accountSecurityAdminOnly = $state(false);
 let linkByVerifiedEmail = $state(true);

 // Passkey
 let passkeyEnabled = $state(false);
 let passkeyLabel = $state('');
 let passkeyRPID = $state('');
 let passkeyRPDisplayName = $state('');
 let passkeyRPOrigins = $state<string[]>([]);
 let passkeyUserVerification = $state('');
 let passkeyChallengeTTLSec = $state<number | ''>('');

 // OAuth2
 let oauth2Entries = $state<OAuth2Entry[]>([]);

 // Sessions
 let cookieName = $state('');
 let cookieDomain = $state('');
 let cookiePath = $state('');
 let cookieSecure = $state(false);
 let cookieDisableHttpOnly = $state(false);
 let cookieSameSite = $state('');
 let issuerAccessTTLSec = $state<number | ''>('');
 let issuerRefreshTTLSec = $state<number | ''>('');
 let issuerDisableRefreshRotation = $state(false);

 // Rate limit
 let rlEnabled = $state(true);
 let rlWindowSec = $state<number | ''>('');
 let rlIPSoft = $state<number | ''>('');
 let rlIPHard = $state<number | ''>('');
 let rlUserSoft = $state<number | ''>('');
 let rlUserHard = $state<number | ''>('');
 let rlBackoffBaseSec = $state<number | ''>('');
 let rlBackoffMaxSec = $state<number | ''>('');
 let rlTrustedProxies = $state<string[]>([]);

 // Capabilities
 let capSuperadmins = $state<string[]>([]);
 let capRoleMappings = $state<MappingRow[]>([]);
 let capScopeMappings = $state<MappingRow[]>([]);

 function loadFromSettings(auth: AuthSettings) {
  base = structuredClone(auth);

  uiTitle = auth.ui?.title ?? '';
  uiSubtitle = auth.ui?.subtitle ?? '';

  registryAnonymousRead = auth.registry_anonymous_read ?? false;

  localEnabled = auth.local?.enabled ?? false;
  localName = auth.local?.name ?? '';
  localLoginFormCollapsed = auth.local?.login_form_collapsed ?? false;
  accountSecurityAdminOnly = auth.account_security_admin_only ?? false;
  linkByVerifiedEmail = auth.link_by_verified_email ?? true;

  passkeyEnabled = auth.passkey?.enabled ?? false;
  passkeyLabel = auth.passkey?.label ?? '';
  passkeyRPID = auth.passkey?.rp_id ?? '';
  passkeyRPDisplayName = auth.passkey?.rp_display_name ?? '';
  passkeyRPOrigins = [...(auth.passkey?.rp_origins ?? [])];
  passkeyUserVerification = auth.passkey?.user_verification ?? '';
  passkeyChallengeTTLSec = nsToSec(auth.passkey?.challenge_ttl);

  oauth2Entries = (auth.oauth2 ?? []).map((e) => ({
   ...e,
   // Never bind the masked secret; carry only the "is set" indicator.
   client_secret: '',
   client_secret_set: e.client_secret_set ?? false,
   clear_client_secret: false,
   scopes: [...(e.scopes ?? [])],
   roles_claims: [...(e.roles_claims ?? [])],
  }));

  cookieName = auth.cookie?.name ?? '';
  cookieDomain = auth.cookie?.domain ?? '';
  cookiePath = auth.cookie?.path ?? '';
  cookieSecure = auth.cookie?.secure ?? false;
  cookieDisableHttpOnly = auth.cookie?.disable_http_only ?? false;
  cookieSameSite = auth.cookie?.same_site ?? '';
  issuerAccessTTLSec = nsToSec(auth.issuer?.access_ttl);
  issuerRefreshTTLSec = nsToSec(auth.issuer?.refresh_ttl);
  issuerDisableRefreshRotation = auth.issuer?.disable_refresh_rotation ?? false;

  const rl = auth.rate_limit;
  rlEnabled = rl?.enabled ?? true;
  rlWindowSec = nsToSec(rl?.window);
  rlIPSoft = rl?.ip_soft_threshold ?? '';
  rlIPHard = rl?.ip_hard_threshold ?? '';
  rlUserSoft = rl?.user_soft_threshold ?? '';
  rlUserHard = rl?.user_hard_threshold ?? '';
  rlBackoffBaseSec = nsToSec(rl?.backoff_base);
  rlBackoffMaxSec = nsToSec(rl?.backoff_max);
  rlTrustedProxies = [...(rl?.trusted_proxy_cidrs ?? [])];

  capSuperadmins = [...(auth.capabilities?.superadmins ?? [])];
  capRoleMappings = mapToRows(auth.capabilities?.role_mapping);
  capScopeMappings = mapToRows(auth.capabilities?.scope_mapping);
 }

 function buildPayload(): AuthSettings {
  const auth: AuthSettings = structuredClone(base);

  auth.ui = { ...(auth.ui ?? {}) };
  if (uiTitle) auth.ui.title = uiTitle; else delete auth.ui.title;
  if (uiSubtitle) auth.ui.subtitle = uiSubtitle; else delete auth.ui.subtitle;

  auth.registry_anonymous_read = registryAnonymousRead;

  auth.local = { enabled: localEnabled };
  if (localName) auth.local.name = localName;
  if (localLoginFormCollapsed) auth.local.login_form_collapsed = true;
  auth.account_security_admin_only = accountSecurityAdminOnly;
  auth.link_by_verified_email = linkByVerifiedEmail;

  const pk: NonNullable<AuthSettings['passkey']> = { ...(auth.passkey ?? {}), enabled: passkeyEnabled };
  pk.label = passkeyLabel || undefined;
  pk.rp_id = passkeyRPID || undefined;
  pk.rp_display_name = passkeyRPDisplayName || undefined;
  pk.rp_origins = passkeyRPOrigins.length > 0 ? [...passkeyRPOrigins] : undefined;
  pk.user_verification = passkeyUserVerification || undefined;
  pk.challenge_ttl = passkeyChallengeTTLSec !== '' ? secToNs(passkeyChallengeTTLSec) : undefined;
  auth.passkey = pk;

  auth.oauth2 = oauth2Entries.map((e) => {
   const entry: OAuth2Entry = { name: e.name.trim() };
   if (e.display_name) entry.display_name = e.display_name;
   if (e.auth_url) entry.auth_url = e.auth_url;
   if (e.token_url) entry.token_url = e.token_url;
   if (e.userinfo_url) entry.userinfo_url = e.userinfo_url;
   if (e.jwks_url) entry.jwks_url = e.jwks_url;
   if (!hasOAuth2ManualEndpoints(e) && e.issuer_url) entry.issuer_url = e.issuer_url;
   if (e.client_id) entry.client_id = e.client_id;
   // Clear wins over a typed value; an untouched blank keeps the stored secret.
   if (e.clear_client_secret) entry.clear_client_secret = true;
   else if (e.client_secret) entry.client_secret = e.client_secret;
   if (e.scopes && e.scopes.length > 0) entry.scopes = e.scopes;
   if (e.roles_claims && e.roles_claims.length > 0) entry.roles_claims = e.roles_claims;
   if (e.disable_pkce) entry.disable_pkce = true;
   if (e.password_flow) entry.password_flow = true;
   if (e.token_auth_method && e.token_auth_method !== 'basic') entry.token_auth_method = e.token_auth_method;
   if (e.auto_create_user) entry.auto_create_user = true;
   return entry;
  });

  const cookie: NonNullable<AuthSettings['cookie']> = {};
  if (cookieName) cookie.name = cookieName;
  if (cookieDomain) cookie.domain = cookieDomain;
  if (cookiePath) cookie.path = cookiePath;
  if (cookieSecure) cookie.secure = true;
  if (cookieDisableHttpOnly) cookie.disable_http_only = true;
  if (cookieSameSite) cookie.same_site = cookieSameSite;
  auth.cookie = cookie;

  const issuer: NonNullable<AuthSettings['issuer']> = {};
  if (issuerAccessTTLSec !== '') issuer.access_ttl = secToNs(issuerAccessTTLSec);
  if (issuerRefreshTTLSec !== '') issuer.refresh_ttl = secToNs(issuerRefreshTTLSec);
  if (issuerDisableRefreshRotation) issuer.disable_refresh_rotation = true;
  auth.issuer = issuer;

  const rl: NonNullable<AuthSettings['rate_limit']> = { enabled: rlEnabled };
  if (rlWindowSec !== '') rl.window = secToNs(rlWindowSec);
  if (rlIPSoft !== '') rl.ip_soft_threshold = Number(rlIPSoft);
  if (rlIPHard !== '') rl.ip_hard_threshold = Number(rlIPHard);
  if (rlUserSoft !== '') rl.user_soft_threshold = Number(rlUserSoft);
  if (rlUserHard !== '') rl.user_hard_threshold = Number(rlUserHard);
  if (rlBackoffBaseSec !== '') rl.backoff_base = secToNs(rlBackoffBaseSec);
  if (rlBackoffMaxSec !== '') rl.backoff_max = secToNs(rlBackoffMaxSec);
  if (rlTrustedProxies.length > 0) rl.trusted_proxy_cidrs = [...rlTrustedProxies];
  auth.rate_limit = rl;

  const caps: NonNullable<AuthSettings['capabilities']> = {};
  if (capSuperadmins.length > 0) caps.superadmins = [...capSuperadmins];
  const roleMap = rowsToMap(capRoleMappings);
  if (roleMap) caps.role_mapping = roleMap;
  const scopeMap = rowsToMap(capScopeMappings);
  if (scopeMap) caps.scope_mapping = scopeMap;
  auth.capabilities = caps;

  return auth;
 }

 const problem = $derived.by(() => {
  const names = oauth2Entries.map((e) => e.name.trim());
  if (names.some((n) => !n)) return 'Every OAuth2 provider needs a name.';
  if (new Set(names).size !== names.length) return 'OAuth2 provider names must be unique.';
  if (!localEnabled && oauth2Entries.length === 0 && !passkeyEnabled && !base.header?.user) return 'Enable at least one sign-in method, or nobody can sign in.';
  return null;
 });

 async function load() {
  loadError = '';
  try {
   const res = await axios.get<AuthSettings>('/api/v1/settings/auth');
   loadFromSettings(res.data ?? {});
   loaded = true;
  } catch (err) {
   loadError = apiServerMessage(err, 'Could not load the authentication settings');
  }
 }

 async function save() {
  if (problem) return;
  saving = true;
  try {
   const res = await axios.put<AuthSettings>('/api/v1/settings/auth', buildPayload());
   if (res.data && typeof res.data === 'object' && 'local' in res.data) loadFromSettings(res.data);
   else await load();
   await appStore.loadInfo();
   addToast('Authentication settings saved', 'success');
  } catch (err) {
   addToast(apiServerMessage(err, 'Could not save the authentication settings'), 'alert');
  } finally {
   saving = false;
  }
 }

 function addOAuth2() {
  oauth2Entries = [...oauth2Entries, { name: '', scopes: ['openid', 'profile', 'email'], roles_claims: [] }];
 }

 function removeOAuth2(i: number) {
  oauth2Entries = oauth2Entries.filter((_, idx) => idx !== i);
 }

 onMount(() => {
  if (appStore.hasPermission('permissions.manage')) void appStore.loadPermissions();
  void load();
 });
</script>

<PanelHeader title="Authentication">
 How people sign in to kutu, how long sessions last, and what external identities are allowed to do. Saving applies the changes immediately.
</PanelHeader>

{#if loadError}
 <div class="leaf p-4 flex items-center justify-between gap-3" role="alert">
  <span class="text-[13px] text-vermilion-700 dark:text-vermilion-300">{loadError}</span>
  <button type="button" class="btn btn-secondary btn-sm" onclick={load}><RotateCcw size={13} /> Retry</button>
 </div>
{:else if !loaded}
 <div class="leaf h-40 animate-pulse" aria-busy="true" aria-label="Loading authentication settings"></div>
{:else}
 <div class="flex flex-col gap-10 pb-20">
  <section>
   <PanelHeader title="Registry access" level={2}>Who may pull from the registries without signing in.</PanelHeader>
   <div class="leaf p-4 flex items-start justify-between gap-4">
    <div class="min-w-0">
     <div class="text-[14px] font-semibold">Anonymous read</div>
     <p class="field-hint mt-0.5 max-w-[65ch]">Let unauthenticated clients pull images and download packages. Publishing and deleting always need a token or a session.</p>
    </div>
    <Switch checked={registryAnonymousRead} label="Allow anonymous registry read" onchange={() => (registryAnonymousRead = !registryAnonymousRead)} />
   </div>
  </section>

  <section>
   <PanelHeader title="Local accounts" level={2}>Usernames and passwords stored in kutu.</PanelHeader>
   <div class="leaf p-4 space-y-4">
    <div class="flex items-start justify-between gap-4">
     <div>
      <div class="text-[14px] font-semibold">Password sign-in</div>
      <p class="field-hint mt-0.5">Turning this off locks out every local user, including you if you signed in with a password.</p>
     </div>
     <Switch checked={localEnabled} label="Enable password sign-in" onchange={() => (localEnabled = !localEnabled)} />
    </div>
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4 pt-4 border-t border-dashed border-slate-300 dark:border-warm-600">
     <label class="field">
      <span class="field-label">Strategy name</span>
      <input type="text" class="input font-mono" bind:value={localName} placeholder="local" disabled={!localEnabled} />
      <span class="field-hint">Used in the login URL. Changing it breaks scripts that post to the old one.</span>
     </label>
     <div class="field justify-center gap-2">
      <label class="check"><input type="checkbox" bind:checked={localLoginFormCollapsed} disabled={!localEnabled} /> Collapse the password form on the login screen</label>
      <span class="field-hint">Useful when most people use single sign-on. A "Local login" control still reveals it.</span>
     </div>
    </div>
    <div class="pt-4 border-t border-slate-200 dark:border-warm-700 space-y-2">
     <label class="check"><input type="checkbox" bind:checked={accountSecurityAdminOnly} /> Only superadmins may manage their own 2FA and passkeys</label>
     <label class="check"><input type="checkbox" bind:checked={linkByVerifiedEmail} /> Link external sign-ins to existing users by verified email</label>
    </div>
   </div>
  </section>

  <section>
   <PanelHeader title="Login screen" level={2}>Branding shown above the sign-in form.</PanelHeader>
   <div class="leaf p-4 grid grid-cols-1 md:grid-cols-2 gap-4">
    <label class="field">
     <span class="field-label">Title</span>
     <input type="text" class="input" bind:value={uiTitle} placeholder="kutu" />
    </label>
    <label class="field">
     <span class="field-label">Subtitle</span>
     <input type="text" class="input" bind:value={uiSubtitle} placeholder="Optional" />
    </label>
   </div>
  </section>

  <section>
   <PanelHeader title="Passkeys" level={2}>Sign in with a device-bound WebAuthn credential. Users enroll their own under Account security.</PanelHeader>
   <div class="leaf p-4 space-y-4">
    <div class="flex items-start justify-between gap-4">
     <div>
      <div class="text-[14px] font-semibold">Passkey sign-in</div>
      <p class="field-hint mt-0.5">Leave the relying-party fields empty to derive them from the request host.</p>
     </div>
     <Switch checked={passkeyEnabled} label="Enable passkey sign-in" onchange={() => (passkeyEnabled = !passkeyEnabled)} />
    </div>
    {#if passkeyEnabled}
     <div class="grid grid-cols-1 md:grid-cols-2 gap-4 pt-4 border-t border-dashed border-slate-300 dark:border-warm-600">
      <label class="field">
       <span class="field-label">Button label</span>
       <input type="text" class="input" bind:value={passkeyLabel} placeholder="Sign in with passkey" />
      </label>
      <label class="field">
       <span class="field-label">User verification</span>
       <select class="input" bind:value={passkeyUserVerification}>
        <option value="">Default (preferred)</option>
        <option value="required">Required</option>
        <option value="preferred">Preferred</option>
        <option value="discouraged">Discouraged</option>
       </select>
      </label>
      <label class="field">
       <span class="field-label">Relying party ID</span>
       <input type="text" class="input font-mono" bind:value={passkeyRPID} placeholder="kutu.example.com" />
       <span class="field-hint">Changing it invalidates every enrolled passkey.</span>
      </label>
      <label class="field">
       <span class="field-label">Relying party name</span>
       <input type="text" class="input" bind:value={passkeyRPDisplayName} placeholder="kutu" />
      </label>
      <div class="field">
       <label class="field-label" for="auth-pk-origins">Allowed origins</label>
       <StringListInput id="auth-pk-origins" values={passkeyRPOrigins} onchange={(v) => (passkeyRPOrigins = v)} placeholder="https://kutu.example.com" noun="origin" />
      </div>
      <label class="field">
       <span class="field-label">Challenge lifetime (seconds)</span>
       <input type="number" min="1" class="input font-mono" bind:value={passkeyChallengeTTLSec} placeholder="300" />
      </label>
     </div>
    {/if}
   </div>
  </section>

  <section>
   <PanelHeader title="OAuth2 providers" level={2}>
    Single sign-on through GitLab, Keycloak, Google or any OAuth2 / OIDC provider.
    {#snippet actions()}
     <button type="button" class="btn btn-secondary btn-sm" onclick={addOAuth2}><Plus size={13} /> Add provider</button>
    {/snippet}
   </PanelHeader>
   {#if oauth2Entries.length === 0}
    <p class="leaf border-dashed px-6 py-8 text-center text-[13px] text-slate-600 dark:text-warm-300">No providers configured.</p>
   {:else}
    <div class="space-y-3">
     {#each oauth2Entries as _, i (i)}
      <OAuth2ProviderCard bind:entry={oauth2Entries[i]} index={i} onRemove={() => removeOAuth2(i)} />
     {/each}
    </div>
   {/if}
  </section>

  <section>
   <PanelHeader title="Capabilities for external identities" level={2}>
    Superadmins bypass every check. External users get capabilities only through the role and scope mappings below.
   </PanelHeader>
   <div class="leaf p-4 space-y-6">
    <div class="field">
     <label class="field-label" for="auth-superadmins">Superadmins</label>
     <p class="field-hint max-w-[70ch]">Identity subjects: the username for local accounts, the <code class="font-mono">sub</code> claim for OAuth2 (often an opaque ID rather than an email).</p>
     <div class="mt-1">
      <StringListInput id="auth-superadmins" values={capSuperadmins} onchange={(v) => (capSuperadmins = v)} placeholder="username or OIDC sub" noun="superadmin" />
     </div>
    </div>
    <div class="pt-5 border-t border-slate-200 dark:border-warm-700">
     <PermissionMappingEditor
      bind:rows={capRoleMappings}
      {availablePermissions}
      label="Role → permissions"
      noun="role"
      placeholder="e.g. developers"
      inputId="auth-role-permissions"
      emptyText="No role mappings. External users get no capabilities unless they are superadmins."
     >
      {#snippet description()}
       A user carrying any listed role (from the OAuth2 <code class="font-mono">roles</code> claim or the configured claim paths) gets the union of the mapped permissions.
      {/snippet}
     </PermissionMappingEditor>
    </div>
    <div class="pt-5 border-t border-slate-200 dark:border-warm-700">
     <PermissionMappingEditor
      bind:rows={capScopeMappings}
      {availablePermissions}
      label="Scope → permissions"
      noun="scope"
      placeholder="e.g. kutu:publish"
      inputId="auth-scope-permissions"
     >
      {#snippet description()}
       Grants permissions to everyone whose token carries an OAuth2 scope. Less common than role mappings.
      {/snippet}
     </PermissionMappingEditor>
    </div>
    {#if !appStore.hasPermission('permissions.manage')}
     <p class="field-hint">You can't list permissions without <code class="font-mono">permissions.manage</code>, so existing mappings show as unknown keys.</p>
    {/if}
   </div>
  </section>

  <section>
   <PanelHeader title="Sessions" level={2}>The session cookie and how long a sign-in lasts.</PanelHeader>
   <div class="leaf p-4 space-y-4">
    <div class="grid grid-cols-1 md:grid-cols-3 gap-4">
     <label class="field">
      <span class="field-label">Cookie name</span>
      <input type="text" class="input font-mono" bind:value={cookieName} placeholder="kutu_session" />
     </label>
     <label class="field">
      <span class="field-label">Domain</span>
      <input type="text" class="input font-mono" bind:value={cookieDomain} placeholder="Request host" />
     </label>
     <label class="field">
      <span class="field-label">Path</span>
      <input type="text" class="input font-mono" bind:value={cookiePath} placeholder="/" />
     </label>
     <label class="field">
      <span class="field-label">SameSite</span>
      <select class="input" bind:value={cookieSameSite}>
       <option value="">Default (Lax)</option>
       <option value="lax">Lax</option>
       <option value="strict">Strict</option>
       <option value="none">None</option>
      </select>
     </label>
     <div class="field md:col-span-2 justify-center gap-2">
      <label class="check"><input type="checkbox" bind:checked={cookieSecure} /> Always mark Secure</label>
      <label class="check"><input type="checkbox" bind:checked={cookieDisableHttpOnly} /> Expose the cookie to JavaScript</label>
     </div>
    </div>
    <p class="field-hint max-w-[70ch]">Secure is set automatically on HTTPS requests; force it only when TLS ends at a proxy that hides the scheme. Keep the cookie HttpOnly: nothing in the UI reads it.</p>
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4 pt-4 border-t border-dashed border-slate-300 dark:border-warm-600">
     <label class="field">
      <span class="field-label">Access lifetime (seconds)</span>
      <input type="number" min="1" class="input font-mono" bind:value={issuerAccessTTLSec} placeholder="900" />
      <span class="field-hint">How often the session is re-checked behind the scenes. Shorter means disabling a user or changing permissions takes effect sooner.</span>
     </label>
     <label class="field">
      <span class="field-label">Refresh lifetime (seconds)</span>
      <input type="number" min="1" class="input font-mono" bind:value={issuerRefreshTTLSec} placeholder="86400" />
      <span class="field-hint">Inactivity longer than this sends the user back to the login screen.</span>
     </label>
    </div>
    <label class="check"><input type="checkbox" bind:checked={issuerDisableRefreshRotation} /> Disable refresh-token rotation</label>
    <p class="field-hint max-w-[70ch]">With rotation off, the refresh lifetime becomes a hard limit from the original sign-in, and a stolen refresh token stays usable until it expires.</p>
   </div>
  </section>

  <section>
   <PanelHeader title="Brute-force protection" level={2}>Slows down, then refuses, repeated failed sign-ins per client IP and per username.</PanelHeader>
   <div class="leaf p-4 space-y-4">
    <div class="flex items-start justify-between gap-4">
     <div>
      <div class="text-[14px] font-semibold">Rate limiting</div>
      <p class="field-hint mt-0.5">Above the soft threshold responses are delayed; above the hard threshold they get HTTP 429. Empty fields use the defaults shown.</p>
     </div>
     <Switch checked={rlEnabled} label="Enable sign-in rate limiting" onchange={() => (rlEnabled = !rlEnabled)} />
    </div>
    {#if rlEnabled}
     <div class="grid grid-cols-2 md:grid-cols-4 gap-4 pt-4 border-t border-dashed border-slate-300 dark:border-warm-600">
      <label class="field"><span class="field-label">Window (s)</span><input type="number" min="1" class="input font-mono" bind:value={rlWindowSec} placeholder="900" /></label>
      <label class="field"><span class="field-label">Backoff base (s)</span><input type="number" min="0" class="input font-mono" bind:value={rlBackoffBaseSec} placeholder="1" /></label>
      <label class="field"><span class="field-label">Backoff max (s)</span><input type="number" min="1" class="input font-mono" bind:value={rlBackoffMaxSec} placeholder="15" /></label>
      <span class="hidden md:block"></span>
      <label class="field"><span class="field-label">IP soft</span><input type="number" min="1" class="input font-mono" bind:value={rlIPSoft} placeholder="3" /></label>
      <label class="field"><span class="field-label">IP hard</span><input type="number" min="1" class="input font-mono" bind:value={rlIPHard} placeholder="30" /></label>
      <label class="field"><span class="field-label">User soft</span><input type="number" min="1" class="input font-mono" bind:value={rlUserSoft} placeholder="3" /></label>
      <label class="field"><span class="field-label">User hard</span><input type="number" min="1" class="input font-mono" bind:value={rlUserHard} placeholder="15" /></label>
     </div>
     <div class="field">
      <label class="field-label" for="auth-rl-proxies">Trusted proxy CIDRs</label>
      <p class="field-hint max-w-[70ch]">Forwarded-for headers are only trusted from these networks. Leave empty when kutu faces the internet directly.</p>
      <div class="mt-1"><StringListInput id="auth-rl-proxies" values={rlTrustedProxies} onchange={(v) => (rlTrustedProxies = v)} placeholder="10.0.0.0/8" noun="CIDR" /></div>
     </div>
     <p class="field-hint">Rate-limit changes take effect after a server restart.</p>
    {/if}
   </div>
  </section>
 </div>

 <div class="savebar sticky bottom-0 -mx-4 sm:-mx-8 px-4 sm:px-8 py-3 flex flex-wrap items-center justify-end gap-3 border-t border-slate-300 dark:border-warm-700">
  {#if problem}<span class="mr-auto text-[13px] text-vermilion-700 dark:text-vermilion-300">{problem}</span>{/if}
  <button type="button" class="btn btn-secondary" onclick={load} disabled={saving}>Discard changes</button>
  <button type="button" class="btn btn-primary" onclick={save} disabled={saving || problem !== null}>{saving ? 'Saving…' : 'Save authentication settings'}</button>
 </div>
{/if}

<style>
 .savebar {
  background: var(--color-slate-100);
 }

 :global(.dark) .savebar {
  background: var(--color-warm-900);
 }
</style>
