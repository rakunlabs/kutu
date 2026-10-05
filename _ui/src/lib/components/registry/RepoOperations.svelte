<script lang="ts">
  // RepoOperations — per-repository operations card: usage counters,
  // retention, promotion, export/import, prefetch and replication.
  import { Loader2, Download, Upload, ArrowRightLeft, Scissors, RotateCw, Activity } from 'lucide-svelte';
  import { addToast } from '@/lib/store/toast.svelte';
  import { confirmAction } from '@/lib/store/confirm.svelte';
  import * as registryAPI from '@/lib/store/registry.svelte';
  import { formatBytes } from '@/lib/format';
  import type { Repository, RegistryUsage, RetentionItem } from './types';

  type Props = {
    namespace: string;
    repo: Repository;
    siblings: Repository[];
    canAdmin: boolean;
    canDelete: boolean;
    onchanged?: () => void | Promise<void>;
  };
  let { namespace, repo, siblings, canAdmin, canDelete, onchanged }: Props = $props();

  let usage = $state<RegistryUsage | null>(null);
  let busy = $state<string | null>(null);
  let plan = $state<RetentionItem[] | null>(null);

  let promoteName = $state('');
  let promoteVersion = $state('');
  let promoteTarget = $state('');

  let importFile = $state<File | null>(null);
  let importOverwrite = $state(false);

  const hasRetention = $derived(
    repo.kind === 'local' &&
      !!(repo.policy?.retention?.keep_last_versions || repo.policy?.retention?.max_version_age_days),
  );

  $effect(() => {
    repo.name;
    namespace;
    usage = null;
    plan = null;
    promoteTarget = siblings[0]?.name ?? '';
    registryAPI.getUsage(repo.type, namespace, repo.name).then((u) => (usage = u)).catch(() => {});
  });

  async function run<T>(key: string, fn: () => Promise<T>): Promise<T | undefined> {
    busy = key;
    try {
      return await fn();
    } catch (err) {
      const body = err instanceof registryAPI.RegistryAPIError && err.body ? `: ${err.body}` : '';
      addToast(`${key} failed${body || `: ${err}`}`, 'alert');
      return undefined;
    } finally {
      busy = null;
    }
  }

  async function planRetention() {
    const out = await run('Retention plan', () => registryAPI.runRetention(repo.type, namespace, repo.name, true));
    if (out) plan = out.deletions ?? [];
  }

  async function applyRetention() {
    if (!plan?.length) return;
    const ok = await confirmAction({
      title: `Delete ${plan.length} version${plan.length === 1 ? '' : 's'}?`,
      message: 'Retention removes the listed versions permanently.',
      confirmLabel: 'Apply retention',
      danger: true,
    });
    if (!ok) return;
    const out = await run('Retention', () => registryAPI.runRetention(repo.type, namespace, repo.name, false));
    if (out) {
      addToast(`Retention deleted ${out.deleted ?? 0} version(s)`, out.errors?.length ? 'alert' : 'success');
      plan = null;
      await onchanged?.();
    }
  }

  async function promote() {
    if (!promoteName || !promoteVersion || !promoteTarget) return;
    const out = await run('Promote', () =>
      registryAPI.promoteVersion(repo.type, namespace, repo.name, {
        name: promoteName.trim(),
        version: promoteVersion.trim(),
        target_repo: promoteTarget,
      }),
    );
    if (out) addToast(`Promoted ${promoteName}@${promoteVersion} → ${promoteTarget}`, 'success');
  }

  async function doImport() {
    if (!importFile) return;
    const f = importFile;
    const out = await run('Import', () => registryAPI.importArchive(repo.type, namespace, repo.name, f, importOverwrite));
    if (out) {
      addToast(`Imported ${out.files} file(s), ${formatBytes(out.bytes)}${out.skipped ? `, ${out.skipped} skipped` : ''}`, 'success');
      importFile = null;
      await onchanged?.();
    }
  }

  async function prefetch() {
    const out = await run('Prefetch', () => registryAPI.prefetchNow(repo.type, namespace, repo.name));
    if (out) addToast(out.errors?.length ? `Prefetch finished with ${out.errors.length} error(s)` : 'Prefetch finished', out.errors?.length ? 'alert' : 'success');
  }

  async function replicate() {
    const out = await run('Replication', () => registryAPI.replicateNow(repo.type, namespace, repo.name));
    if (out) {
      addToast(`Replicated ${out.files} file(s), ${formatBytes(out.bytes)}`, 'success');
      await onchanged?.();
    }
  }
</script>

{#if repo.kind !== 'virtual' && (canAdmin || usage)}
  <div class="mb-4 leaf p-3 space-y-3">
    <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 flex items-center gap-1.5">
      <Activity size={11} /> Usage &amp; operations
    </div>

    {#if usage}
      <div class="grid grid-cols-3 gap-2 text-[13px]">
        <div>
          <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Requests</div>
          <div class="font-mono">{usage.requests}</div>
        </div>
        <div>
          <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Errors</div>
          <div class="font-mono">{usage.errors}</div>
        </div>
        <div>
          <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Served</div>
          <div class="font-mono">{formatBytes(usage.bytes_served)}</div>
        </div>
      </div>
      {#if usage.top_downloads?.length}
        <details>
          <summary class="cursor-pointer text-[12px] text-slate-500 dark:text-warm-400">Top downloads since restart</summary>
          <ul class="mt-1 text-[12px] font-mono space-y-0.5">
            {#each usage.top_downloads as d (d.package)}
              <li class="flex justify-between gap-2"><span class="truncate">{d.package}</span><span>{d.downloads}</span></li>
            {/each}
          </ul>
        </details>
      {/if}
    {/if}

    {#if canAdmin}
      <div class="flex flex-wrap gap-2">
        <a class="btn btn-secondary btn-sm" href={registryAPI.exportURL(repo.type, namespace, repo.name)} download>
          <Download size={12} /> Export
        </a>
        {#if repo.kind === 'remote'}
          <button class="btn btn-secondary btn-sm" disabled={!!busy || !repo.prefetch?.packages?.length} onclick={prefetch}
            title={repo.prefetch?.packages?.length ? 'Warm the configured packages now' : 'Configure prefetch packages in the repository policies first'}>
            {#if busy === 'Prefetch'}<Loader2 size={12} class="animate-spin" />{:else}<RotateCw size={12} />{/if}
            Prefetch now
          </button>
        {/if}
        {#if repo.kind === 'local' && repo.replication?.source_url}
          <button class="btn btn-secondary btn-sm" disabled={!!busy} onclick={replicate}>
            {#if busy === 'Replication'}<Loader2 size={12} class="animate-spin" />{:else}<RotateCw size={12} />{/if}
            Replicate now
          </button>
        {/if}
        {#if hasRetention && canDelete}
          <button class="btn btn-secondary btn-sm" disabled={!!busy} onclick={planRetention}>
            {#if busy === 'Retention plan'}<Loader2 size={12} class="animate-spin" />{:else}<Scissors size={12} />{/if}
            Plan retention
          </button>
        {/if}
      </div>

      {#if plan}
        <div class="rounded border border-slate-200 dark:border-warm-700 p-2 text-[12px]">
          {#if plan.length === 0}
            Nothing to delete under the current retention policy.
          {:else}
            <div class="mb-1">{plan.length} version{plan.length === 1 ? '' : 's'} would be deleted:</div>
            <ul class="font-mono max-h-40 overflow-y-auto space-y-0.5">
              {#each plan as it (it.name + '@' + it.version)}
                <li class="flex justify-between gap-2"><span class="truncate">{it.name}@{it.version}</span><span class="text-slate-500">{it.reason}</span></li>
              {/each}
            </ul>
            <button class="btn btn-danger btn-sm mt-2" disabled={!!busy} onclick={applyRetention}>
              {#if busy === 'Retention'}<Loader2 size={12} class="animate-spin" />{/if}
              Apply
            </button>
          {/if}
        </div>
      {/if}

      {#if repo.kind === 'local'}
        <details>
          <summary class="cursor-pointer text-[12px] text-slate-500 dark:text-warm-400">Import an export archive</summary>
          <div class="flex flex-wrap items-center gap-2 mt-2">
            <input type="file" accept=".tar.gz,.tgz,application/gzip" class="text-[12px]"
              onchange={(e) => (importFile = e.currentTarget.files?.[0] ?? null)} />
            <label class="flex items-center gap-1 text-[12px]">
              <input type="checkbox" bind:checked={importOverwrite} /> overwrite
            </label>
            <button class="btn btn-secondary btn-sm" disabled={!importFile || !!busy} onclick={doImport}>
              {#if busy === 'Import'}<Loader2 size={12} class="animate-spin" />{:else}<Upload size={12} />{/if}
              Import
            </button>
          </div>
        </details>

        {#if siblings.length > 0}
          <details>
            <summary class="cursor-pointer text-[12px] text-slate-500 dark:text-warm-400">Promote a version to another repository</summary>
            <div class="grid grid-cols-1 sm:grid-cols-4 gap-2 mt-2">
              <input class="input font-mono sm:col-span-2" placeholder="package name" bind:value={promoteName} />
              <input class="input font-mono" placeholder="version" bind:value={promoteVersion} />
              <select class="input" bind:value={promoteTarget}>
                {#each siblings as s (s.name)}<option value={s.name}>{s.name}</option>{/each}
              </select>
            </div>
            <button class="btn btn-secondary btn-sm mt-2" disabled={!promoteName || !promoteVersion || !!busy} onclick={promote}>
              {#if busy === 'Promote'}<Loader2 size={12} class="animate-spin" />{:else}<ArrowRightLeft size={12} />{/if}
              Promote
            </button>
          </details>
        {/if}
      {/if}
    {/if}
  </div>
{/if}
