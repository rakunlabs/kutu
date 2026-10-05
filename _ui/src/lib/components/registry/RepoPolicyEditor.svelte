<script lang="ts">
  // RepoPolicyEditor — policy, signing, prefetch and replication fields
  // of the repository form. Edits `draft` in place; Registries.svelte
  // prunes empty values on save.
  import type { Repository } from './types';
  import { protocol } from './protocols';

  let { draft = $bindable() }: { draft: Repository } = $props();

  const p = $derived(protocol(draft.type));
  const isLocal = $derived(draft.kind === 'local');
  const isRemote = $derived(draft.kind === 'remote');
  const osvTypes = ['go', 'npm', 'maven', 'pypi', 'cargo', 'nuget', 'rubygems', 'composer', 'pub', 'swift', 'cran', 'conan'];
  const supportsOSV = $derived(osvTypes.includes(draft.type));

  function ensurePolicy() {
    draft.policy ??= {};
    return draft.policy;
  }
  function ensureRetention() {
    const pol = ensurePolicy();
    pol.retention ??= {};
    return pol.retention;
  }

  const list = (v?: string[]) => (v ?? []).join(', ');
  const parse = (s: string) => s.split(',').map((x) => x.trim()).filter(Boolean);

  let quotaGB = $state(0);
  $effect.pre(() => {
    quotaGB = draft.policy?.quota_bytes ? +(draft.policy.quota_bytes / 1024 ** 3).toFixed(2) : 0;
  });
  function setQuota(v: number) {
    quotaGB = v;
    ensurePolicy().quota_bytes = v > 0 ? Math.round(v * 1024 ** 3) : undefined;
  }
</script>

<details class="border-t border-slate-200 dark:border-warm-700 pt-3 mt-3">
  <summary class="cursor-pointer text-[13px] font-medium text-warm-600 dark:text-warm-400 hover:text-warm-900 dark:hover:text-warm-100">
    Policies (access, security, retention)
  </summary>
  <div class="space-y-4 mt-3">
    <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
      <label class="block">
        <span class="field-label block mb-1.5">Include packages</span>
        <input type="text" class="input font-mono" placeholder="@acme/**, com/acme/**"
          value={list(draft.policy?.include)}
          onchange={(e) => (ensurePolicy().include = parse(e.currentTarget.value))} />
        <span class="field-hint block mt-1">Only matching names are served. Empty = all.</span>
      </label>
      <label class="block">
        <span class="field-label block mb-1.5">Exclude packages</span>
        <input type="text" class="input font-mono" placeholder="event-stream, colors"
          value={list(draft.policy?.exclude)}
          onchange={(e) => (ensurePolicy().exclude = parse(e.currentTarget.value))} />
        <span class="field-hint block mt-1">Always blocked. <code>**</code> matches any depth.</span>
      </label>
    </div>

    <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
      <label class="block">
        <span class="field-label block mb-1.5">Quarantine new versions (days)</span>
        <input type="number" min="0" class="input font-mono" placeholder="0"
          value={draft.policy?.quarantine_days ?? 0}
          onchange={(e) => (ensurePolicy().quarantine_days = Number(e.currentTarget.value) || undefined)} />
        <span class="field-hint block mt-1">Hide versions published less than N days ago.</span>
      </label>
      {#if supportsOSV}
        <div class="block">
          <span class="field-label block mb-1.5">Vulnerabilities (OSV)</span>
          <label class="flex items-center gap-2 cursor-pointer text-[13px]">
            <input type="checkbox" checked={draft.policy?.block_vulnerable === true}
              onchange={(e) => (ensurePolicy().block_vulnerable = e.currentTarget.checked)} />
            Block versions with known advisories
          </label>
          <select class="input mt-1.5" value={draft.policy?.min_severity ?? ''}
            disabled={!draft.policy?.block_vulnerable}
            onchange={(e) => (ensurePolicy().min_severity = e.currentTarget.value as never)}>
            <option value="">Any severity</option>
            <option value="low">Low and above</option>
            <option value="moderate">Moderate and above</option>
            <option value="high">High and above</option>
            <option value="critical">Critical only</option>
          </select>
        </div>
      {/if}
    </div>

    <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
      <label class="block">
        <span class="field-label block mb-1.5">Allowed licenses</span>
        <input type="text" class="input font-mono" placeholder="MIT, Apache-2.0, BSD-3-Clause"
          value={list(draft.policy?.allowed_licenses)}
          onchange={(e) => (ensurePolicy().allowed_licenses = parse(e.currentTarget.value))} />
      </label>
      <label class="block">
        <span class="field-label block mb-1.5">Denied licenses</span>
        <input type="text" class="input font-mono" placeholder="AGPL-3.0, SSPL-1.0"
          value={list(draft.policy?.denied_licenses)}
          onchange={(e) => (ensurePolicy().denied_licenses = parse(e.currentTarget.value))} />
      </label>
    </div>
    <label class="flex items-center gap-2 cursor-pointer text-[13px]">
      <input type="checkbox" checked={draft.policy?.block_unknown_license === true}
        onchange={(e) => (ensurePolicy().block_unknown_license = e.currentTarget.checked)} />
      Block packages without license metadata
    </label>

    {#if draft.type === 'docker'}
      <label class="flex items-center gap-2 cursor-pointer text-[13px]">
        <input type="checkbox" checked={draft.policy?.require_signature === true}
          onchange={(e) => (ensurePolicy().require_signature = e.currentTarget.checked)} />
        Require a cosign / notation signature to pull by tag
      </label>
    {/if}

    {#if isLocal}
      <div class="rounded border border-slate-200 dark:border-warm-700 p-3 space-y-3">
        <div class="text-[13px] font-medium text-warm-700 dark:text-warm-300">Publishing &amp; retention</div>
        <label class="flex items-center gap-2 cursor-pointer text-[13px]">
          <input type="checkbox" checked={draft.policy?.immutable_versions === true}
            onchange={(e) => (ensurePolicy().immutable_versions = e.currentTarget.checked)} />
          Immutable versions (reject re-publishing an existing version)
        </label>
        <div class="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <label class="block">
            <span class="field-label block mb-1.5">Quota (GB)</span>
            <input type="number" min="0" step="0.5" class="input font-mono" value={quotaGB}
              onchange={(e) => setQuota(Number(e.currentTarget.value) || 0)} />
          </label>
          <label class="block">
            <span class="field-label block mb-1.5">Keep last N</span>
            <input type="number" min="0" class="input font-mono" value={draft.policy?.retention?.keep_last_versions ?? 0}
              onchange={(e) => (ensureRetention().keep_last_versions = Number(e.currentTarget.value) || undefined)} />
          </label>
          <label class="block">
            <span class="field-label block mb-1.5">Max age (days)</span>
            <input type="number" min="0" class="input font-mono" value={draft.policy?.retention?.max_version_age_days ?? 0}
              onchange={(e) => (ensureRetention().max_version_age_days = Number(e.currentTarget.value) || undefined)} />
          </label>
          <label class="block">
            <span class="field-label block mb-1.5">Always keep</span>
            <input type="text" class="input font-mono" placeholder="v1.*, *-lts"
              value={list(draft.policy?.retention?.keep_patterns)}
              onchange={(e) => (ensureRetention().keep_patterns = parse(e.currentTarget.value))} />
          </label>
        </div>
        <span class="field-hint block">Retention never deletes a package's newest version. Run it from the repository's Operations card.</span>
      </div>

      {#if p.signing}
        <div class="rounded border border-slate-200 dark:border-warm-700 p-3 space-y-2">
          <div class="text-[13px] font-medium text-warm-700 dark:text-warm-300">Metadata signing key</div>
          <textarea class="input font-mono text-[12px]" rows="4"
            placeholder={p.signing === 'pgp' ? '-----BEGIN PGP PRIVATE KEY BLOCK-----' : '-----BEGIN RSA PRIVATE KEY-----'}
            bind:value={draft.signing_key}></textarea>
          {#if p.signing === 'rsa'}
            <input type="text" class="input font-mono" placeholder="key name (default: kutu)" bind:value={draft.signing_key_name} />
          {/if}
          <span class="field-hint block">
            {p.signing === 'pgp'
              ? 'Unprotected ASCII-armored OpenPGP private key. Signs Release/InRelease (apt) or repomd.xml (rpm).'
              : 'PEM RSA private key used to sign APKINDEX.'}
            Sealed with the at-rest key; leave empty for unsigned metadata.
          </span>
        </div>
      {/if}

      <div class="rounded border border-slate-200 dark:border-warm-700 p-3 space-y-2">
        <div class="text-[13px] font-medium text-warm-700 dark:text-warm-300">Pull replication</div>
        <input type="url" class="input font-mono" placeholder="https://other-kutu/api/v1/registries/{draft.type}/NS/REPO/export"
          value={draft.replication?.source_url ?? ''}
          onchange={(e) => (draft.replication = { ...(draft.replication ?? { source_url: '' }), source_url: e.currentTarget.value })} />
        <div class="grid grid-cols-2 gap-2">
          <input type="password" class="input font-mono" placeholder="source token"
            value={draft.replication?.token ?? ''}
            onchange={(e) => (draft.replication = { ...(draft.replication ?? { source_url: '' }), token: e.currentTarget.value })} />
          <input type="text" class="input font-mono" placeholder="interval (1h)"
            value={draft.replication?.interval ?? ''}
            onchange={(e) => (draft.replication = { ...(draft.replication ?? { source_url: '' }), interval: e.currentTarget.value })} />
        </div>
        <span class="field-hint block">Mirrors another kutu repository of the same type on a schedule (incremental).</span>
      </div>
    {/if}

    {#if isRemote}
      <div class="rounded border border-slate-200 dark:border-warm-700 p-3 space-y-2">
        <div class="text-[13px] font-medium text-warm-700 dark:text-warm-300">Scheduled prefetch</div>
        <textarea class="input font-mono text-[12px]" rows="3" placeholder={'lodash\nreact@18.3.1'}
          value={(draft.prefetch?.packages ?? []).join('\n')}
          onchange={(e) => (draft.prefetch = { ...(draft.prefetch ?? {}), packages: e.currentTarget.value.split('\n').map((x) => x.trim()).filter(Boolean) })}></textarea>
        <input type="text" class="input font-mono" placeholder="interval (6h)"
          value={draft.prefetch?.interval ?? ''}
          onchange={(e) => (draft.prefetch = { ...(draft.prefetch ?? {}), interval: e.currentTarget.value })} />
        <span class="field-hint block">One package per line (<code>name</code> or <code>name@version</code>); kept warm in the cache.</span>
      </div>
    {/if}
  </div>
</details>
