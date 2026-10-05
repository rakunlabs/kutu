<!--
 OAuth2ProviderCard — editor for one OAuth2 provider entry in the
 Authentication settings. Mutates the bound entry in place.
-->
<script lang="ts">
 import { Trash2 } from 'lucide-svelte';
 import { hasOAuth2ManualEndpoints, type OAuth2Entry } from './types';
 import StringListInput from './StringListInput.svelte';

 type Props = {
  entry: OAuth2Entry;
  index: number;
  onRemove: () => void;
 };

 let { entry = $bindable(), index, onRemove }: Props = $props();

 const uid = $props.id();
 const p = `auth-oauth2-${uid}`;

</script>

<div class="rounded-[3px] border border-slate-300 dark:border-warm-600 p-4 space-y-4">
 <div class="flex items-center justify-between gap-2">
  <span class="text-[14px] font-semibold text-slate-900 dark:text-warm-50">{entry.display_name || entry.name || `Provider ${index + 1}`}</span>
  <button type="button" class="btn btn-danger-ghost btn-sm" onclick={onRemove} aria-label="Remove provider {index + 1}"><Trash2 size={13} /> Remove</button>
 </div>

 <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
  <label class="field">
   <span class="field-label">Name</span>
   <input id="{p}-name" type="text" class="input font-mono" bind:value={entry.name} placeholder="gitlab" />
   <span class="field-hint">Used in the callback URL: <code class="font-mono">/login/callback/{entry.name || '<name>'}</code></span>
  </label>
  <label class="field">
   <span class="field-label">Button label</span>
   <input id="{p}-display" type="text" class="input" bind:value={entry.display_name} placeholder="Sign in with GitLab" />
  </label>
  <label class="field">
   <span class="field-label">Authorization URL</span>
   <input type="text" class="input font-mono" bind:value={entry.auth_url} placeholder="https://gitlab.com/oauth/authorize" />
  </label>
  <label class="field">
   <span class="field-label">Token URL</span>
   <input type="text" class="input font-mono" bind:value={entry.token_url} placeholder="https://gitlab.com/oauth/token" />
  </label>
  <label class="field">
   <span class="field-label">UserInfo URL</span>
   <input type="text" class="input font-mono" bind:value={entry.userinfo_url} placeholder="https://gitlab.com/oauth/userinfo" />
   <span class="field-hint">Fetches identity claims with the access token. Use it when there is no JWKS URL.</span>
  </label>
  <label class="field">
   <span class="field-label">JWKS URL</span>
   <input type="text" class="input font-mono" bind:value={entry.jwks_url} placeholder="Provider's jwks_uri" />
   <span class="field-hint">Public keys that verify the <code class="font-mono">id_token</code>, from the provider's <code class="font-mono">.well-known/openid-configuration</code>.</span>
  </label>
 </div>
 <p class="field-hint">Set at least one of JWKS URL or UserInfo URL, otherwise kutu cannot resolve who signed in.</p>
 {#if entry.issuer_url && !hasOAuth2ManualEndpoints(entry)}
  <p class="text-[12px] status-warn">This provider still uses a legacy issuer URL. Fill in the Authorization and Token URLs to stop using discovery.</p>
 {/if}

 <div class="grid grid-cols-1 md:grid-cols-2 gap-4 pt-4 border-t border-dashed border-slate-300 dark:border-warm-600">
  <label class="field">
   <span class="field-label">Client ID</span>
   <input type="text" class="input font-mono" bind:value={entry.client_id} autocomplete="off" />
  </label>
  <div class="field">
   <label class="field-label" for="{p}-secret">Client secret</label>
   <input
    id="{p}-secret"
    type="password"
    class="input font-mono"
    bind:value={entry.client_secret}
    disabled={entry.clear_client_secret}
    autocomplete="new-password"
    placeholder={entry.clear_client_secret ? 'Will be cleared on save' : entry.client_secret_set ? 'Stored. Leave empty to keep it' : 'No secret stored'}
   />
   {#if entry.client_secret_set}
    <label class="check !text-[12px]">
     <input
      type="checkbox"
      checked={entry.clear_client_secret}
      onchange={(ev) => {
       entry.clear_client_secret = ev.currentTarget.checked;
       if (entry.clear_client_secret) entry.client_secret = '';
      }}
     />
     Clear the stored secret
    </label>
   {/if}
  </div>
  <label class="field md:col-span-2">
   <span class="field-label">Client authentication</span>
   <select class="input" value={entry.token_auth_method || 'basic'} onchange={(ev) => (entry.token_auth_method = ev.currentTarget.value)}>
    <option value="basic">HTTP Basic header (client_secret_basic, default)</option>
    <option value="post">Request parameters (client_secret_post)</option>
    <option value="bearer">Bearer token (Authorization: Bearer)</option>
   </select>
   <span class="field-hint">Try <code class="font-mono">client_secret_post</code> if sign-in fails with "invalid_client" even though the secret is right.</span>
  </label>
 </div>

 <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
  <div class="field">
   <label class="field-label" for="{p}-scopes">Scopes</label>
   <StringListInput id="{p}-scopes" values={entry.scopes ?? []} onchange={(v) => (entry.scopes = v)} placeholder="openid" noun="scope" />
  </div>
  <div class="field">
   <label class="field-label" for="{p}-roles">Roles claim paths</label>
   <StringListInput id="{p}-roles" values={entry.roles_claims ?? []} onchange={(v) => (entry.roles_claims = v)} placeholder="realm_access.roles" noun="claim path" />
   <span class="field-hint">Empty reads the <code class="font-mono">roles</code> claim. Nesting and <code class="font-mono">*</code> work, e.g. <code class="font-mono">resource_access.*.roles</code> for Keycloak.</span>
  </div>
 </div>

 <div class="flex flex-wrap gap-x-5 gap-y-2">
  <label class="check"><input type="checkbox" bind:checked={entry.disable_pkce} /> Disable PKCE</label>
  <label class="check"><input type="checkbox" bind:checked={entry.password_flow} /> Password flow</label>
  <label class="check"><input type="checkbox" bind:checked={entry.auto_create_user} /> Create users on first sign-in</label>
 </div>
 <p class="field-hint">With user creation on, an unknown identity becomes an external-only kutu user. Existing linked users and verified-email matches are reused first.</p>
</div>
