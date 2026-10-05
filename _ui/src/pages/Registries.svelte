<script lang="ts">
  // Registries — top-level page for the artifact registry feature
  // (Go modules, NPM packages, Docker / OCI images).
  //
  // ── Scope of this page ────────────────────────────────────────────
  // Phase 1 surface: namespace + repository listing, type/kind
  // badges and per-repo artifact browser. Local repos additionally
  // show an upload guide (manual `curl PUT` for now; a form-based
  // uploader is queued for a later phase).
  //
  // ── Layout ────────────────────────────────────────────────────────
  //   ┌────────────┬────────────────┬────────────────────────────┐
  //   │ namespaces │ repositories   │ detail panel               │
  //   │ list       │ list (filter)  │ (modules, versions, CLI)   │
  //   └────────────┴────────────────┴────────────────────────────┘
  //
  // ── Endpoints used ────────────────────────────────────────────────
  //   GET  /api/v1/registries                          → namespace tree
  //   GET  /api/v1/registries/repos                    → flat list
  //   GET  /api/v1/registries/go/{ns}/{repo}/modules   → modules + versions

  import { confirmAction } from '@/lib/store/confirm.svelte';
  import { onMount } from 'svelte';
  import { link } from 'svelte-spa-router';
  import { appStore } from '@/lib/store/store.svelte';
  import { addToast } from '@/lib/store/toast.svelte';
  import { configStore } from '@/lib/store/config.svelte';
  import { rawMountsStore } from '@/lib/store/rawmounts.svelte';
  import {
    Loader2,
    Copy, ChevronRight, FolderTree, Trash2, Plus, Pencil, X, RotateCw,
    Eye, EyeOff, Search,
    // These four are used directly in the template for per-protocol
    // section headers and empty states; iconFor() / kindIcon() in
    // registry/utils.ts cover the badge-icon callsites.
    Package, FileBox, Container, Anchor,
  } from 'lucide-svelte';
  import { basePath } from '@/lib/basepath';
  import { formatBytes } from '@/lib/format';
  import * as registryAPI from '@/lib/store/registry.svelte';
  import Modal from '@/lib/components/Modal.svelte';
  import PackageDetailPanel from '@/lib/components/registry/PackageDetailPanel.svelte';
  import RepoPolicyEditor from '@/lib/components/registry/RepoPolicyEditor.svelte';
  import RepoOperations from '@/lib/components/registry/RepoOperations.svelte';
  import RegistrySearch from '@/lib/components/registry/RegistrySearch.svelte';
  import { PROTOCOLS, protocol, fillSnippet, snippetVars } from '@/lib/components/registry/protocols';
  import type {
    CargoCrateEntry,
    DockerEntry,
    DockerTag,
    HelmChartListEntry,
    MavenArtifactEntry,
    ModuleEntry,
    Namespace,
    PackageEntry,
    PackageSummary,
    ProbeResult,
    PyPIPackageEntry,
    RegistryStats,
    RegistryType,
    RegistryUpstream,
    Repository,
    UpstreamAuth,
  } from '@/lib/components/registry/types';
  import { LEGACY_TYPES } from '@/lib/components/registry/types';
  import {
    artifactTypeLabel,
    copyToClipboard as copyToClipboardUtil,
    endpointURL as endpointURLUtil,
    iconFor,
    kindIcon,
    matchFilter,
  } from '@/lib/components/registry/utils';

  let booted = $state(false);
  let settingsLoadRequested = $state(false);
  let namespaces = $state<Namespace[]>([]);
  let selectedNS = $state<string | null>(null);
  let selectedRepo = $state<Repository | null>(null);
  let modules = $state<ModuleEntry[]>([]);
  let packages = $state<PackageEntry[]>([]);
  let images = $state<DockerEntry[]>([]);
  let charts = $state<HelmChartListEntry[]>([]);
  let mavenArtifacts = $state<MavenArtifactEntry[]>([]);
  let pypiPackages = $state<PyPIPackageEntry[]>([]);
  let cargoCrates = $state<CargoCrateEntry[]>([]);
  let genericEntries = $state<PackageSummary[]>([]);
  let searchOpen = $state(false);
  let entriesLoading = $state(false);
  // For all three browsers — only one is shown at a time depending
  // on selectedRepo.type, so a single expanded-name key is enough.
  let expandedEntry = $state<string | null>(null);

  // PackageDetailPanel state. When `detailPackage` is non-null, the
  // panel renders as a right-side overlay; clicking a package row
  // sets this and clicking the close button on the panel clears it.
  // Kept at the page level (rather than per-browser) because the
  // panel is a singleton — only one package can be "open" at a time.
  let detailPackage = $state<{ name: string; type: RegistryType } | null>(null);
  function openDetail(name: string, type: RegistryType) {
    detailPackage = { name, type };
  }
  function closeDetail() {
    detailPackage = null;
  }

  async function refreshAfterArtifactDelete() {
    if (selectedNS && selectedRepo) {
      await loadEntries(selectedNS, selectedRepo);
      await loadStats(selectedNS, selectedRepo);
    }
  }

  // Per-list text filters. Each browser's list is filtered against
  // its respective state below; an empty filter shows everything.
  // The filter inputs render inside the browser headers (see template
  // below) and update these states in place.
  let nsFilter = $state('');
  let repoFilter = $state('');
  let entryFilter = $state('');

  const canAdmin = $derived(appStore.hasPermission('registry.admin'));
  const canDelete = $derived(appStore.hasPermission('registry.delete'));
  // Server-side feature toggle. When disabled, the data plane and
  // every /api/v1/registries/* endpoint return 404, so we render a
  // dedicated empty-state instead of letting load() spam "Failed to
  // load registries" toasts on each visit. Default true so users
  // on an older server build (no registry_enabled field) keep the
  // existing behaviour.
  const registryEnabled = $derived(appStore.info?.registry_enabled ?? true);

  async function load() {
    // Skip the fetch entirely when the feature is off — the API
    // would 404 and the user is already going to see the explicit
    // "feature disabled" panel below.
    if (!registryEnabled) {
      booted = true;
      return;
    }
    try {
      const body = await registryAPI.listRegistries();
      namespaces = body.namespaces ?? [];
      if (namespaces.length > 0 && !selectedNS) {
        selectedNS = namespaces[0].name;
      }
    } catch (err) {
      addToast(`Failed to load registries: ${err}`, 'alert');
    } finally {
      booted = true;
    }
  }

  async function loadEntries(ns: string, repo: Repository) {
    modules = [];
    packages = [];
    images = [];
    charts = [];
    mavenArtifacts = [];
    pypiPackages = [];
    cargoCrates = [];
    genericEntries = [];
    entriesLoading = true;
    try {
      // Dispatch on protocol — each API call has a different
      // response shape and a different state slot. The switch
      // keeps the call narrow and type-safe; the alternative
      // (a generic listEntries) would lose the per-protocol
      // typing the rest of the page depends on.
      switch (repo.type) {
        case 'go':
          modules = await registryAPI.listGoModules(ns, repo.name);
          break;
        case 'npm':
          packages = await registryAPI.listNPMPackages(ns, repo.name);
          break;
        case 'docker':
          images = await registryAPI.listDockerRepos(ns, repo.name);
          break;
        case 'helm':
          charts = await registryAPI.listHelmCharts(ns, repo.name);
          break;
        case 'maven':
          mavenArtifacts = await registryAPI.listMavenArtifacts(ns, repo.name);
          break;
        case 'pypi':
          pypiPackages = await registryAPI.listPyPIPackages(ns, repo.name);
          break;
        case 'cargo':
          cargoCrates = await registryAPI.listCargoCrates(ns, repo.name);
          break;
        default:
          genericEntries = await registryAPI.listEntries(repo.type, ns, repo.name);
          break;
      }
    } catch (err) {
      addToast(`Failed to load entries: ${err}`, 'alert');
    } finally {
      entriesLoading = false;
    }
  }

  function selectRepo(repo: Repository) {
    selectedRepo = repo;
    expandedEntry = null;
    if (selectedNS) {
      loadEntries(selectedNS, repo);
      loadStats(selectedNS, repo);
    }
  }

  // Thin local wrappers — the page uses these names extensively in
  // the template, so we re-bind the utils versions to the original
  // local identifiers rather than touching dozens of callsites.
  const copyToClipboard = (text: string) => copyToClipboardUtil(text, addToast);

  // Per-repo on-disk statistics. Lazy-loaded the first time a repo
  // is selected, then re-fetched whenever the repo changes. Server
  // walks the storage on every call (no persistent counter), so we
  // keep the call rate low — refresh button instead of polling.
  let stats = $state<RegistryStats | null>(null);
  let statsLoading = $state(false);
  async function loadStats(ns: string, repo: Repository) {
    stats = null;
    if (repo.kind === 'virtual') {
      // Virtual repos delegate to members. The server returns {}
      // for them; surface a clean "n/a" rather than the empty
      // object so the UI doesn't render a card full of zeros.
      return;
    }
    statsLoading = true;
    try {
      stats = await registryAPI.getStats(repo.type, ns, repo.name);
    } catch (err) {
      // Non-fatal — the panel just hides the card.
      stats = null;
    } finally {
      statsLoading = false;
    }
  }

  // humanBytes is the format-with-zero variant used by the stats
  // card. Renamed from a local function to the shared formatBytes
  // helper but kept as an alias to avoid a template-wide rename.
  const humanBytes = formatBytes;
  const defaultGCMinAgeSeconds = 3600;
  const defaultAbandonedUploadMaxAgeSeconds = 86400;
  function effectiveGCMinAge(repo: Repository): number {
    return repo.policy?.retention?.gc_min_age_seconds || defaultGCMinAgeSeconds;
  }
  function effectiveAbandonedUploadMaxAge(repo: Repository): number {
    return repo.policy?.retention?.abandoned_upload_max_age_seconds || defaultAbandonedUploadMaxAgeSeconds;
  }
  function formatSeconds(seconds: number): string {
    if (seconds <= 0) return '0s';
    if (seconds % 86400 === 0) return `${seconds / 86400}d`;
    if (seconds % 3600 === 0) return `${seconds / 3600}h`;
    if (seconds % 60 === 0) return `${seconds / 60}m`;
    return `${seconds}s`;
  }

  // Cache purge for Remote repos. Mirrors the GC pattern: confirm,
  // POST, toast the stats, then reload the per-repo browser so any
  // freshly-dropped entries disappear immediately. Default scope is
  // "mutable" — operator can opt into a full purge through the
  // confirm prompt.
  let purgeRunning = $state(false);
  async function runCachePurge(ns: string, repo: Repository) {
    const scope = await confirmAction({
      title: `Refresh the upstream cache of ${ns}/${repo.name}?`,
      message: 'Drops cached version lists and floating tags, so the next read asks the upstream again. Cached artifacts stay.',
      confirmLabel: 'Refresh cache',
    });
    if (!scope) return;
    const all = await confirmAction({
      title: 'Also drop cached artifacts?',
      message: 'Manifests, tarballs and blobs are deleted too and every pull re-downloads them. Most refreshes do not need this.',
      confirmLabel: 'Drop artifacts too',
      cancelLabel: 'Keep artifacts',
      danger: true,
    });
    purgeRunning = true;
    try {
      const stats = await registryAPI.purgeCache(repo.type, ns, repo.name, all) as {
        purged_bytes?: number;
        purged_files?: number;
      };
      const bytes = stats.purged_bytes ?? 0;
      const human = bytes >= 1024 * 1024
        ? `${(bytes / (1024 * 1024)).toFixed(1)} MB`
        : `${(bytes / 1024).toFixed(0)} KB`;
      addToast(
        `Purged ${stats.purged_files} file${stats.purged_files === 1 ? '' : 's'} (${human}).`,
        'success'
      );
      if (selectedRepo && selectedNS) {
        await loadEntries(selectedNS, selectedRepo);
      }
    } catch (err) {
      addToast(`Cache purge failed: ${err}`, 'alert');
    } finally {
      purgeRunning = false;
    }
  }

  // GC for Docker Local repos. Confirms with the user, runs the
  // server-side sweep, surfaces the resulting stats via toast,
  // then reloads the image list so freed-up entries disappear.
  //
  // kutu never runs GC on a schedule: cleanup is operator-driven
  // here, with a side channel of cheap delete-time cascading in
  // the registry head. The estimate panel below this function
  // surfaces the reclaimable size BEFORE committing to a run.
  let gcRunning = $state(false);
  let gcEstimate = $state<registryAPI.GCStats | null>(null);
  let gcEstimateLoading = $state(false);
  let gcEstimateRepoKey = $state<string | null>(null); // "ns/repo" the estimate belongs to

  // refreshGCEstimate runs a dry-run mark-and-sweep and caches the
  // result for the currently-viewed Docker Local repo. Cheap to
  // re-trigger; we DO NOT auto-refresh on every selection because
  // the dry-run walk is still O(blob count).
  async function refreshGCEstimate(ns: string, repoName: string) {
    gcEstimate = null;
    gcEstimateLoading = true;
    gcEstimateRepoKey = `${ns}/${repoName}`;
    try {
      gcEstimate = await registryAPI.dockerGCEstimate(ns, repoName);
    } catch (err) {
      addToast(`Garbage estimate failed: ${err}`, 'alert');
    } finally {
      gcEstimateLoading = false;
    }
  }

  async function runDockerGC(ns: string, repoName: string) {
    const grace = selectedRepo && selectedRepo.name === repoName
      ? formatSeconds(effectiveGCMinAge(selectedRepo))
      : 'the policy grace window';
    if (!(await confirmAction({ title: `Run garbage collection on ${ns}/${repoName}?`, message: `Unreferenced blobs and manifests are deleted. Anything newer than ${grace} is kept.`, confirmLabel: 'Run cleanup', danger: true }))) {
      return;
    }
    gcRunning = true;
    try {
      const stats = await registryAPI.dockerGC(ns, repoName);
      const totalBytes = stats.swept_bytes + stats.abandoned_uploads_bytes;
      const msg = `Swept ${stats.swept_blobs} blob${stats.swept_blobs === 1 ? '' : 's'}, ` +
                  `${stats.swept_manifests} manifest${stats.swept_manifests === 1 ? '' : 's'}, ` +
                  `${stats.abandoned_uploads_removed} stale upload${stats.abandoned_uploads_removed === 1 ? '' : 's'} ` +
                  `(${formatBytes(totalBytes)} reclaimed).`;
      addToast(msg, 'success');
      // The estimate is now stale — refresh in the background so
      // the panel doesn't keep displaying yesterday's numbers.
      if (gcEstimateRepoKey === `${ns}/${repoName}`) {
        await refreshGCEstimate(ns, repoName);
      }
      if (selectedRepo && selectedNS) {
        await loadEntries(selectedNS, selectedRepo);
      }
    } catch (err) {
      addToast(`GC failed: ${err}`, 'alert');
    } finally {
      gcRunning = false;
    }
  }

  // endpointURL wraps the shared util so the template keeps its
  // 2-arg shape (basePath is bound here at the page level).
  const endpointURL = (ns: string, repo: string) => endpointURLUtil(basePath, ns, repo);

  const defaultBasePath = (type: RegistryType) => protocol(type).basePath;
  const defaultCachePath = (type: RegistryType) => protocol(type).cachePath;
  const defaultUpstreamURL = (type: RegistryType) => protocol(type).upstream;

  // ─── Admin actions: CRUD over namespaces + repositories ───
  //
  // The server takes a whole-tree PUT (action=set on the registry
  // block), so every mutation goes through the same recipe:
  //   1. Mutate the local `namespaces` array (creates a new tree).
  //   2. POST the whole tree via configStore.saveRegistrySettings.
  //   3. Re-load from the API to pick up server-canonicalized form.
  // We keep "disabled" intact across saves by reading it from
  // configStore.settings (the feature flag must NOT silently flip).

  // Modal state. `mode` controls which dialog is open; null = none.
  type Mode =
    | { kind: 'new-namespace' }
    | { kind: 'edit-namespace'; original: string }
    | { kind: 'new-repository'; namespace: string }
    | { kind: 'edit-repository'; namespace: string; original: string };
  let mode = $state<Mode | null>(null);
  let saving = $state(false);

  // ── Mode discriminator helpers ──
  //
  // Svelte 5's reactive `$derived.by` closures and template-side
  // ternaries on a 4-variant union don't always narrow cleanly in
  // svelte-check — the type checker can't follow `mode.kind ===
  // 'new-repository' || mode.kind === 'edit-repository'` to prove
  // that `.namespace` exists. These helpers do the narrowing in
  // one place so call sites can use a single accessor without
  // non-null assertions.
  function modeNamespace(m: Mode | null): string {
    if (!m) return '';
    if (m.kind === 'new-repository' || m.kind === 'edit-repository') return m.namespace;
    return '';
  }
  function modeOriginal(m: Mode | null): string {
    if (!m) return '';
    if (m.kind === 'edit-namespace' || m.kind === 'edit-repository') return m.original;
    return '';
  }
  function isRepoMode(m: Mode | null): m is
    | { kind: 'new-repository'; namespace: string }
    | { kind: 'edit-repository'; namespace: string; original: string } {
    return !!m && (m.kind === 'new-repository' || m.kind === 'edit-repository');
  }
  function isNamespaceMode(m: Mode | null): m is
    | { kind: 'new-namespace' }
    | { kind: 'edit-namespace'; original: string } {
    return !!m && (m.kind === 'new-namespace' || m.kind === 'edit-namespace');
  }

  // Draft buffers — separate from the live tree so cancel is cheap
  // (just clear `mode`).
  let nsDraft = $state<Namespace>({ name: '', description: '', repositories: [] });
  let repoDraft = $state<Repository>({
    name: '',
    description: '',
    type: 'go',
    kind: 'local',
    allow_push: true,
  });
  // Virtual-members editor uses a comma-separated string for the
  // simplest possible input. Round-tripped through .split/.join
  // when opening / saving.
  let virtualMembersText = $state('');
  // Floating-tags editor (Docker Remote only) — same comma-string
  // convention. Empty input means "use server default".
  let floatingTagsText = $state('');
  // CORS origins editor — comma-separated origins string. Same
  // pattern as virtualMembersText. The backend stores them as a
  // string slice; the UI keeps the editor flat for keyboard
  // efficiency (one input vs a chip array with per-chip buttons).
  let corsOriginsText = $state('');
  // Max upload size editor: number + unit dropdown. We persist the
  // bytes value in repoDraft.max_upload_size; the inputs below
  // expose a friendlier MB/GB unit picker. Zero (or empty) means
  // "type default" — preserved by omitting the field at save time.
  let maxUploadValue = $state(0);
  let maxUploadUnit = $state<'MB' | 'GB'>('MB');

  // Docker Local policy editor. Immutable tags are a comma-separated
  // glob list; retention numbers are persisted as seconds. Zero means
  // "server default" so existing repos keep the historical 1h/24h GC
  // behaviour until an operator opts into different defaults.
  let immutableTagsText = $state('');
  let gcMinAgeSeconds = $state(0);
  let abandonedUploadMaxAgeSeconds = $state(0);

  // Reveal toggles for the credential inputs. We render the fields
  // as type="password" by default — the value comes back plaintext
  // from the server (the seal layer transparently injects it) but
  // the browser masks it. Click 👁 to flip the input to type=text.
  // Reset to false each time the modal opens.
  let revealPassword = $state(false);
  let revealToken = $state(false);
  let revealHeaderValue = $state(false);

  // Upstream probe state. Lives at the page level (rather than
  // inside the modal subtree) so a long-running probe survives
  // briefly closing/reopening the modal — and so the result panel
  // can be cleared when the modal opens fresh.
  let probeResult = $state<ProbeResult | null>(null);
  let probeRunning = $state(false);
  async function runProbe() {
    if (!isRepoMode(mode)) return;
    const ns = mode.namespace;
    const repoName = (repoDraft.name ?? '').trim().toLowerCase();
    if (!repoName) {
      addToast('Save the repository once before testing the upstream.', 'alert');
      return;
    }
    probeRunning = true;
    probeResult = null;
    try {
      probeResult = await registryAPI.probeUpstream(repoDraft.type, ns, repoName);
    } catch (err) {
      probeResult = { ok: false, error: String(err) };
    } finally {
      probeRunning = false;
    }
  }

  // rawMounts feeds the mount dropdown for Local + Remote repos.
  // Loaded from /api/v1/raw-mounts via the shared store (see onMount).
  const rawMounts = $derived(rawMountsStore.mounts);

  // ── Prefix-routed upstreams editor (Go remote only) ──
  function addUpstream() {
    repoDraft.upstreams = [...(repoDraft.upstreams ?? []), { prefix: '', url: '' }];
  }
  function removeUpstream(i: number) {
    repoDraft.upstreams = (repoDraft.upstreams ?? []).filter((_, idx) => idx !== i);
  }
  function setUpstreamAuthType(up: RegistryUpstream, v: string) {
    if (!v) up.auth = undefined;
    else up.auth = { ...(up.auth ?? {}), type: v as 'basic' | 'bearer' | 'header' };
  }

  function openNewNamespace() {
    nsDraft = { name: '', description: '', repositories: [] };
    mode = { kind: 'new-namespace' };
  }

  function openEditNamespace(ns: Namespace) {
    nsDraft = { name: ns.name, description: ns.description ?? '', repositories: ns.repositories ?? [] };
    mode = { kind: 'edit-namespace', original: ns.name };
  }

  function openNewRepository(namespaceName: string) {
    repoDraft = {
      name: '',
      description: '',
      type: 'go',
      kind: 'local',
      allow_push: true,
    };
    virtualMembersText = '';
    floatingTagsText = '';
    corsOriginsText = '';
    maxUploadValue = 0;
    maxUploadUnit = 'MB';
    immutableTagsText = '';
    gcMinAgeSeconds = 0;
    abandonedUploadMaxAgeSeconds = 0;
    revealPassword = false;
    revealToken = false;
    revealHeaderValue = false;
    probeResult = null;
    mode = { kind: 'new-repository', namespace: namespaceName };
  }

  function openEditRepository(namespaceName: string, repo: Repository) {
    repoDraft = JSON.parse(JSON.stringify(repo)); // deep copy
    virtualMembersText = (repo.members ?? []).join(', ');
    floatingTagsText = (repo.floating_tags ?? []).join(', ');
    corsOriginsText = (repo.cors_origins ?? []).join(', ');
    immutableTagsText = (repo.policy?.immutable_tags ?? []).join(', ');
    gcMinAgeSeconds = repo.policy?.retention?.gc_min_age_seconds ?? 0;
    abandonedUploadMaxAgeSeconds = repo.policy?.retention?.abandoned_upload_max_age_seconds ?? 0;
    // Reconstruct the friendly value+unit pair from the stored
    // bytes. Round-trip prefers GB when the value is a clean
    // multiple, else MB. Sub-MB values (operator typed bytes
    // directly through a previous version) round up to 1 MB for
    // the display; saving overwrites with the new (value * unit).
    const bytes = repo.max_upload_size ?? 0;
    if (bytes <= 0) {
      maxUploadValue = 0;
      maxUploadUnit = 'MB';
    } else if (bytes >= 1024 * 1024 * 1024 && bytes % (1024 * 1024 * 1024) === 0) {
      maxUploadValue = bytes / (1024 * 1024 * 1024);
      maxUploadUnit = 'GB';
    } else {
      maxUploadValue = Math.max(1, Math.round(bytes / (1024 * 1024)));
      maxUploadUnit = 'MB';
    }
    revealPassword = false;
    revealToken = false;
    revealHeaderValue = false;
    mode = { kind: 'edit-repository', namespace: namespaceName, original: repo.name };
  }

  function cancelModal() {
    mode = null;
  }

  // commitTree posts the namespaces array via configStore. We always
  // include the existing `disabled` bit so saving from this page
  // never accidentally turns the feature off (or back on).
  async function commitTree(newNamespaces: Namespace[]) {
    saving = true;
    try {
      const disabled = configStore.settings?.registry?.disabled === true;
      await configStore.saveRegistrySettings({ disabled, namespaces: newNamespaces });
      namespaces = newNamespaces;
      addToast('Registry configuration saved.', 'success');
      mode = null;
      // Reload to pick up server-side defaults (e.g. default
      // MutableTTL filled in by validator). Best-effort — local
      // tree is the source of truth until reload completes.
      await load();
    } catch (err) {
      // saveRegistrySettings already surfaced a toast with the
      // server's error detail. Keep the dialog open so the user
      // can correct the input and retry.
      // eslint-disable-next-line no-console
      console.error('commitTree failed', err);
    } finally {
      saving = false;
    }
  }

  async function saveNamespace() {
    const m = mode;
    if (!isNamespaceMode(m)) return;
    const name = (nsDraft.name ?? '').trim().toLowerCase();
    if (!name) {
      addToast('Namespace name is required.', 'alert');
      return;
    }
    if (!/^[a-z0-9_-]+$/.test(name)) {
      addToast('Namespace name must match [a-z0-9_-]+.', 'alert');
      return;
    }
    const tree = JSON.parse(JSON.stringify(namespaces)) as Namespace[];
    if (m.kind === 'new-namespace') {
      if (tree.some((n) => n.name === name)) {
        addToast(`Namespace "${name}" already exists.`, 'alert');
        return;
      }
      tree.push({ name, description: nsDraft.description, repositories: [] });
    } else {
      const original = m.original;
      const idx = tree.findIndex((n) => n.name === original);
      if (idx === -1) {
        addToast('Original namespace not found — tree was edited elsewhere.', 'alert');
        return;
      }
      if (name !== original && tree.some((n) => n.name === name)) {
        addToast(`Namespace "${name}" already exists.`, 'alert');
        return;
      }
      tree[idx] = { ...tree[idx], name, description: nsDraft.description };
    }
    await commitTree(tree);
    if (mode === null) {
      selectedNS = name; // jump to the new/renamed namespace
    }
  }

  async function deleteNamespace(name: string) {
    const ns = namespaces.find((n) => n.name === name);
    const repoCount = ns?.repositories?.length ?? 0;
    const ok = await confirmAction({
      title: repoCount > 0 ? `Delete namespace "${name}" and its ${repoCount} repositor${repoCount === 1 ? 'y' : 'ies'}?` : `Delete namespace "${name}"?`,
      message: repoCount > 0 ? 'Only the routing configuration is removed. Artifacts in the backing raw mount stay on disk.' : undefined,
      confirmLabel: 'Delete namespace',
      danger: true,
    });
    if (!ok) return;
    const tree = namespaces.filter((n) => n.name !== name);
    if (selectedNS === name) {
      selectedNS = tree.length > 0 ? tree[0].name : null;
      selectedRepo = null;
    }
    await commitTree(tree);
  }

  async function saveRepository() {
    const m = mode;
    if (!isRepoMode(m)) return;
    const name = (repoDraft.name ?? '').trim().toLowerCase();
    if (!name) {
      addToast('Repository name is required.', 'alert');
      return;
    }
    if (!/^[a-z0-9_-]+$/.test(name)) {
      addToast('Repository name must match [a-z0-9_-]+.', 'alert');
      return;
    }
    // Per-kind validation. Server re-validates; this is just UX.
    if (repoDraft.kind === 'local') {
      if (!repoDraft.mount) {
        addToast('Local repository requires a mount.', 'alert');
        return;
      }
      if (!repoDraft.base_path) {
        addToast('Local repository requires a base path.', 'alert');
        return;
      }
      if (repoDraft.type === 'docker') {
        const immutableTags = immutableTagsText.split(',').map((s) => s.trim()).filter(Boolean);
        if (immutableTags.some((s) => s.includes('/'))) {
          addToast('Immutable tag patterns must match tags, not paths.', 'alert');
          return;
        }
        if (Number(gcMinAgeSeconds) < 0 || Number(abandonedUploadMaxAgeSeconds) < 0) {
          addToast('Retention windows must be zero or positive seconds.', 'alert');
          return;
        }
      }
    } else if (repoDraft.kind === 'remote') {
      if (!repoDraft.url) {
        addToast('Remote repository requires an upstream URL.', 'alert');
        return;
      }
      if (!repoDraft.mount) {
        addToast('Remote repository requires a cache mount.', 'alert');
        return;
      }
      if (!repoDraft.base_path) {
        addToast('Remote repository requires a cache base path.', 'alert');
        return;
      }
    } else if (repoDraft.kind === 'virtual') {
      const members = virtualMembersText.split(',').map((s) => s.trim()).filter(Boolean);
      if (members.length === 0) {
        addToast('Virtual repository requires at least one member.', 'alert');
        return;
      }
      repoDraft.members = members;
    }

    const tree = JSON.parse(JSON.stringify(namespaces)) as Namespace[];
    const nsIdx = tree.findIndex((n) => n.name === m.namespace);
    if (nsIdx === -1) {
      addToast('Target namespace not found.', 'alert');
      return;
    }
    const ns = tree[nsIdx];
    ns.repositories = ns.repositories ?? [];

    // Build the row to persist — strip fields that don't apply to
    // this kind so the on-disk shape stays clean.
    const row: Repository = {
      name,
      description: repoDraft.description?.trim() || undefined,
      type: repoDraft.type,
      kind: repoDraft.kind,
    };
    if (repoDraft.kind === 'local') {
      row.mount = repoDraft.mount;
      row.base_path = repoDraft.base_path;
      row.allow_push = repoDraft.allow_push !== false;
      if (repoDraft.type === 'docker') {
        const immutableTags = immutableTagsText.split(',').map((s) => s.trim()).filter(Boolean);
        const gcMinAge = Math.floor(Number(gcMinAgeSeconds) || 0);
        const uploadMaxAge = Math.floor(Number(abandonedUploadMaxAgeSeconds) || 0);
        const retention: { gc_min_age_seconds?: number; abandoned_upload_max_age_seconds?: number } = {};
        if (gcMinAge > 0) retention.gc_min_age_seconds = gcMinAge;
        if (uploadMaxAge > 0) retention.abandoned_upload_max_age_seconds = uploadMaxAge;
        if (immutableTags.length > 0 || Object.keys(retention).length > 0) {
          row.policy = {};
          if (immutableTags.length > 0) row.policy.immutable_tags = immutableTags;
          if (Object.keys(retention).length > 0) row.policy.retention = retention;
        }
      }
    } else if (repoDraft.kind === 'remote') {
      row.url = repoDraft.url;
      row.mount = repoDraft.mount;
      row.base_path = repoDraft.base_path;
      if (repoDraft.mutable_ttl) row.mutable_ttl = repoDraft.mutable_ttl;
      if (repoDraft.insecure_skip_verify) row.insecure_skip_verify = true;
      // FloatingTags is Docker-only; the server silently ignores it
      // for Go / NPM but we keep the on-disk shape clean by not
      // writing it for other types. Empty input means "server
      // default" — preserved by omitting the field entirely.
      if (repoDraft.type === 'docker') {
        const ft = floatingTagsText.split(',').map((s) => s.trim()).filter(Boolean);
        if (ft.length > 0) row.floating_tags = ft;
      }
      // Auth: only include the type-relevant fields. Secrets are
      // entered directly; the server seals them with the at-rest key
      // before persistence.
      if (repoDraft.auth?.type) {
        row.auth = { type: repoDraft.auth.type };
        if (repoDraft.auth.type === 'basic') {
          row.auth.username = repoDraft.auth.username;
          row.auth.password = repoDraft.auth.password;
        } else if (repoDraft.auth.type === 'bearer') {
          row.auth.token = repoDraft.auth.token;
        } else if (repoDraft.auth.type === 'header') {
          row.auth.header = repoDraft.auth.header;
          row.auth.value = repoDraft.auth.value;
        }
      }
      // Prefix-routed upstreams (Go only). Trim blanks; the backend
      // validates prefix/url and seals per-upstream secrets + ssh keys.
      if (protocol(repoDraft.type).prefixUpstreams && repoDraft.upstreams?.length) {
        const ups = repoDraft.upstreams
          .map((u): RegistryUpstream => {
            const out: RegistryUpstream = {
              prefix: (u.prefix ?? '').trim(),
              url: (u.url ?? '').trim(),
            };
            if (u.auth?.type) {
              out.auth = { type: u.auth.type };
              if (u.auth.type === 'basic') {
                out.auth.username = u.auth.username;
                out.auth.password = u.auth.password;
              } else if (u.auth.type === 'bearer') {
                out.auth.token = u.auth.token;
              } else if (u.auth.type === 'header') {
                out.auth.header = u.auth.header;
                out.auth.value = u.auth.value;
              }
            }
            if (u.ssh_key?.trim()) out.ssh_key = u.ssh_key;
            return out;
          })
          .filter((u) => u.prefix || u.url);
        if (ups.length > 0) row.upstreams = ups;
      }
    } else if (repoDraft.kind === 'virtual') {
      row.members = repoDraft.members;
      if (repoDraft.default_local) row.default_local = repoDraft.default_local;
    }

    // Policy, signing, prefetch and replication come from
    // RepoPolicyEditor, which edits repoDraft in place. Docker's
    // immutable tags / GC fields (built above) are merged in.
    const policy = { ...(repoDraft.policy ?? {}), ...(row.policy ?? {}) };
    if (repoDraft.kind !== 'local' || repoDraft.type !== 'docker') {
      delete policy.immutable_tags;
    }
    if (policy.retention && repoDraft.kind === 'local' && repoDraft.type === 'docker') {
      policy.retention = { ...(repoDraft.policy?.retention ?? {}), ...(row.policy?.retention ?? {}) };
    } else if (policy.retention) {
      delete policy.retention.gc_min_age_seconds;
      delete policy.retention.abandoned_upload_max_age_seconds;
    }
    const cleanPolicy = pruneEmpty(policy);
    if (cleanPolicy) row.policy = cleanPolicy;
    else delete row.policy;
    if (repoDraft.kind === 'local' && protocol(repoDraft.type).signing) {
      if (repoDraft.signing_key?.trim()) row.signing_key = repoDraft.signing_key;
      if (repoDraft.signing_key_name?.trim()) row.signing_key_name = repoDraft.signing_key_name.trim();
    }
    if (repoDraft.kind === 'remote' && repoDraft.prefetch?.packages?.length) {
      row.prefetch = { packages: repoDraft.prefetch.packages, interval: repoDraft.prefetch.interval || undefined };
    }
    if (repoDraft.kind === 'local' && repoDraft.replication?.source_url?.trim()) {
      row.replication = { ...repoDraft.replication, source_url: repoDraft.replication.source_url.trim() };
    }

    // Common per-repo overrides — applicable across kinds, so live
    // outside the kind-switch. Empty inputs are dropped to preserve
    // the canonical "field absent" shape over "field empty".
    const cors = corsOriginsText
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean);
    if (cors.length > 0) row.cors_origins = cors;
    if (maxUploadValue && maxUploadValue > 0) {
      const mult = maxUploadUnit === 'GB' ? 1024 * 1024 * 1024 : 1024 * 1024;
      row.max_upload_size = Math.floor(maxUploadValue) * mult;
    }

    if (m.kind === 'new-repository') {
      if (ns.repositories.some((r) => r.name === name)) {
        addToast(`Repository "${name}" already exists in this namespace.`, 'alert');
        return;
      }
      ns.repositories.push(row);
    } else {
      const original = m.original;
      const rIdx = ns.repositories.findIndex((r) => r.name === original);
      if (rIdx === -1) {
        addToast('Original repository not found.', 'alert');
        return;
      }
      if (name !== original && ns.repositories.some((r) => r.name === name)) {
        addToast(`Repository "${name}" already exists in this namespace.`, 'alert');
        return;
      }
      ns.repositories[rIdx] = row;
    }
    await commitTree(tree);
  }

  // pruneEmpty drops empty strings/arrays/false/0 and empty objects so
  // the persisted policy stays in the canonical "field absent" form.
  function pruneEmpty<T>(v: T): T | undefined {
    if (Array.isArray(v)) return (v.length ? v : undefined) as T | undefined;
    if (v && typeof v === 'object') {
      const out: Record<string, unknown> = {};
      for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
        const p = pruneEmpty(val);
        if (p !== undefined) out[k] = p;
      }
      return (Object.keys(out).length ? out : undefined) as T | undefined;
    }
    if (v === '' || v === false || v === 0 || v === null) return undefined;
    return v;
  }

  async function deleteRepository(namespaceName: string, repoName: string) {
    if (!(await confirmAction({ title: `Delete repository "${namespaceName}/${repoName}"?`, message: 'Only the routing configuration is removed. Artifacts in the backing raw mount stay on disk.', confirmLabel: 'Delete repository', danger: true }))) {
      return;
    }
    const tree = JSON.parse(JSON.stringify(namespaces)) as Namespace[];
    const ns = tree.find((n) => n.name === namespaceName);
    if (!ns) return;
    ns.repositories = (ns.repositories ?? []).filter((r) => r.name !== repoName);
    if (selectedRepo?.name === repoName && selectedNS === namespaceName) {
      selectedRepo = null;
    }
    await commitTree(tree);
  }

  // Sibling-repo list for the virtual-members hint. Helps the user
  // type valid names without leaving the page.
  const siblingRepoNames = $derived.by(() => {
    const m = mode;
    if (!isRepoMode(m)) return [];
    const ns = namespaces.find((n) => n.name === m.namespace);
    return (ns?.repositories ?? [])
      .filter((r) => r.kind !== 'virtual')
      .filter((r) => m.kind !== 'edit-repository' || r.name !== m.original)
      .map((r) => r.name);
  });

  const selectedRepos = $derived.by(() => {
    if (!selectedNS) return [];
    const ns = namespaces.find((n) => n.name === selectedNS);
    const all = ns?.repositories ?? [];
    if (!repoFilter) return all;
    return all.filter((r) => matchFilter(r.name, repoFilter));
  });
  const selectedReposTotal = $derived.by(() => {
    if (!selectedNS) return 0;
    const ns = namespaces.find((n) => n.name === selectedNS);
    return ns?.repositories?.length ?? 0;
  });

  onMount(async () => {
    await load();
    // Mount pickers in the repo editor need the configured raw mounts.
    void rawMountsStore.load();
  });

  $effect(() => {
    // Settings are needed so admin saves preserve the registry disabled
    // flag. canAdmin can be false on the first refresh tick while
    // /api/v1/info is still loading, so trigger this when it becomes true.
    if (!canAdmin || settingsLoadRequested) return;
    settingsLoadRequested = true;
    void configStore.loadSettings();
  });
</script>

<div class="flex flex-col h-full bg-warm-50 dark:bg-warm-950 text-warm-900 dark:text-warm-100">
  <!-- Header strip -->
  <header class="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 px-4 py-2.5 border-b border-slate-300 dark:border-warm-700 bg-white dark:bg-warm-900">
    <div class="flex items-baseline gap-3 min-w-0">
      <h1 class="text-[17px] font-bold tracking-tight">Registries</h1>
      <span class="hidden lg:inline text-[13px] text-slate-500 dark:text-warm-400 truncate">
        {PROTOCOLS.length} package formats
      </span>
    </div>
    <div class="flex items-center gap-2">
      <button class="btn btn-secondary btn-sm" onclick={() => (searchOpen = true)} title="Search packages across every repository">
        <Search size={13} />
        Search
      </button>
      {#if canAdmin}
        <button
          class="btn btn-primary btn-sm"
          onclick={openNewNamespace}
          title="Create a new namespace"
        >
          <Plus size={13} />
          New namespace
        </button>
      {/if}
    </div>
  </header>

  {#if !booted}
    <div class="flex-1 flex items-center justify-center text-slate-500 dark:text-warm-400">
      <Loader2 size={20} class="animate-spin mr-2" />
      Loading registries…
    </div>
  {:else if !registryEnabled}
    <!--
      Feature is server-disabled. Operators reach this page through a
      bookmark or direct URL even after the navbar link is hidden;
      give them a single self-service hint instead of a stack of
      failed-to-load toasts.
    -->
    <div class="flex-1 flex items-center justify-center">
      <div class="max-w-md text-center px-4">
        <Package size={48} class="mx-auto text-slate-400 dark:text-warm-500 mb-4" />
        <h2 class="text-lg font-semibold mb-2">Registry feature is disabled</h2>
        <p class="text-[14px] text-slate-500 dark:text-warm-400 mb-4">
          An administrator has turned off the artifact registry for this deployment.
          Existing namespaces and repositories are preserved — re-enable the feature
          to make them serve again.
        </p>
        <a
          href="/settings"
          use:link
          class="btn btn-secondary"
        >
          Open Settings
        </a>
      </div>
    </div>
  {:else if namespaces.length === 0}
    <div class="flex-1 flex items-center justify-center">
      <div class="max-w-md text-center px-4">
        <Package size={48} class="mx-auto text-slate-400 dark:text-warm-500 mb-4" />
        <h2 class="text-lg font-semibold mb-2">No registries configured yet</h2>
        <p class="text-[14px] text-slate-500 dark:text-warm-400 mb-4">
          kutu can host {PROTOCOLS.length} package formats — from Go, npm, Docker and Maven
          to NuGet, APT, RPM, Terraform and Hugging Face — using
          your existing raw mounts as storage. Each namespace groups local,
          remote (upstream-proxied) and virtual (aggregated) repositories.
        </p>
        {#if canAdmin}
          <button
            class="btn btn-primary"
            onclick={openNewNamespace}
          >
            <Plus size={14} />
            Create your first namespace
          </button>
        {:else}
          <p class="text-[13px] text-slate-500 dark:text-warm-400">
            Ask an administrator with the <span class="font-mono">registry.admin</span>
            capability to configure namespaces and repositories.
          </p>
        {/if}
      </div>
    </div>
  {:else}
    <div class="flex-1 flex flex-col md:flex-row overflow-hidden">
      <!-- Namespaces sidebar -->
      <aside class="w-full md:w-48 max-h-36 md:max-h-none border-b md:border-b-0 md:border-r border-slate-300 dark:border-warm-700 bg-slate-50 dark:bg-warm-950/60 overflow-y-auto shrink-0" aria-label="Namespaces">
        <div class="label-caps px-3 pt-3 pb-1.5 text-[11px] text-slate-500 dark:text-warm-400 flex items-center justify-between">
          <span>Namespaces</span>
          <span class="font-mono tracking-normal">{namespaces.length}</span>
        </div>
        {#if namespaces.length > 5}
          <div class="px-2 pb-2">
            <input
              type="text"
              placeholder="Filter…"
              bind:value={nsFilter}
              class="input btn-sm"
            />
          </div>
        {/if}
        <ul class="px-1.5 pb-3">
          {#each namespaces.filter((n) => matchFilter(n.name, nsFilter)) as ns (ns.name)}
            <li class="group relative">
              <button
                class="sel w-full text-left pl-3 pr-2 py-2 rounded-[3px] text-[14px] flex items-center justify-between cursor-pointer"
                class:is-sel={selectedNS === ns.name}
                aria-current={selectedNS === ns.name ? 'true' : undefined}
                onclick={() => { selectedNS = ns.name; selectedRepo = null; }}
              >
                <span class="font-medium truncate">{ns.name}</span>
                <span class="font-mono text-[12px] text-slate-500 dark:text-warm-400 shrink-0 ml-2 group-hover:opacity-0">
                  {ns.repositories?.length ?? 0}
                </span>
              </button>
              {#if canAdmin}
                <div class="absolute right-1 top-1/2 -translate-y-1/2 flex gap-0.5 opacity-0 group-hover:opacity-100 group-focus-within:opacity-100">
                  <button
                    class="btn btn-ghost btn-sm btn-icon !h-6 !w-6"
                    title="Edit namespace"
                    aria-label={`Edit namespace ${ns.name}`}
                    onclick={(e) => { e.stopPropagation(); openEditNamespace(ns); }}
                  >
                    <Pencil size={11} />
                  </button>
                  <button
                    class="btn btn-danger-ghost btn-sm btn-icon !h-6 !w-6"
                    title="Delete namespace"
                    aria-label={`Delete namespace ${ns.name}`}
                    onclick={(e) => { e.stopPropagation(); deleteNamespace(ns.name); }}
                  >
                    <Trash2 size={11} />
                  </button>
                </div>
              {/if}
            </li>
          {/each}
        </ul>
      </aside>

      <!-- Repositories panel -->
      <section class="w-full md:w-80 flex-1 md:flex-none border-r border-slate-300 dark:border-warm-700 bg-white dark:bg-warm-900 overflow-y-auto md:shrink-0 {selectedRepo || isRepoMode(mode) ? 'hidden md:block' : ''}" aria-label="Repositories">
        <div class="label-caps px-3 pt-3 pb-1.5 text-[11px] text-slate-500 dark:text-warm-400 flex items-center justify-between">
          <span>Repositories {#if selectedReposTotal > 0}<span class="font-mono tracking-normal">{selectedRepos.length}{repoFilter ? ' / ' + selectedReposTotal : ''}</span>{/if}</span>
          {#if canAdmin && selectedNS}
            <button
              class="btn btn-secondary btn-sm !h-6 normal-case tracking-normal"
              style="font-stretch: 100%"
              onclick={() => openNewRepository(selectedNS!)}
              title="Add a new repository to {selectedNS}"
            >
              <Plus size={12} />
              Add
            </button>
          {/if}
        </div>
        {#if selectedReposTotal > 5}
          <div class="px-2 pb-2">
            <input
              type="text"
              placeholder="Filter repositories…"
              bind:value={repoFilter}
              class="input btn-sm"
            />
          </div>
        {/if}
        {#if selectedRepos.length === 0}
          <div class="px-3 py-3 text-[13px] text-slate-600 dark:text-warm-300">
            This namespace has no repositories yet.
            {#if canAdmin && selectedNS}
              <button
                class="btn btn-primary btn-sm mt-3"
                onclick={() => openNewRepository(selectedNS!)}
              >
                + Add repository
              </button>
            {/if}
          </div>
        {:else}
          <ul class="border-t border-slate-200 dark:border-warm-700">
            {#each selectedRepos as repo (repo.name)}
              {@const TypeIcon = iconFor(repo.type)}
              {@const KindIcon = kindIcon(repo.kind)}
              <li class="group relative">
                <button
                  class="sel w-full text-left px-3 py-2.5 border-b border-slate-200 dark:border-warm-700 cursor-pointer"
                  class:is-sel={selectedRepo?.name === repo.name}
                  aria-current={selectedRepo?.name === repo.name ? 'true' : undefined}
                  onclick={() => selectRepo(repo)}
                >
                  <div class="flex items-center gap-2">
                    <TypeIcon size={15} class="text-accent-600 dark:text-accent-300 shrink-0" />
                    <span class="text-[14px] font-semibold truncate">{repo.name}</span>
                    <span class="tag ml-auto shrink-0 group-hover:invisible">
                      <KindIcon size={10} />
                      {repo.kind}
                    </span>
                  </div>
                  <div class="text-[12px] text-slate-500 dark:text-warm-400 mt-1 font-mono truncate">
                    {repo.type}
                    {#if repo.kind === 'local'}· {repo.mount}{/if}
                    {#if repo.kind === 'remote'}· {repo.url}{/if}
                  </div>
                </button>
                {#if canAdmin}
                  <div class="absolute right-2 top-2 flex gap-0.5 opacity-0 group-hover:opacity-100 group-focus-within:opacity-100">
                    <button
                      class="btn btn-ghost btn-sm btn-icon !h-6 !w-6"
                      title="Edit repository"
                      aria-label={`Edit repository ${repo.name}`}
                      onclick={(e) => { e.stopPropagation(); openEditRepository(selectedNS!, repo); }}
                    >
                      <Pencil size={11} />
                    </button>
                    <button
                      class="btn btn-danger-ghost btn-sm btn-icon !h-6 !w-6"
                      title="Delete repository"
                      aria-label={`Delete repository ${repo.name}`}
                      onclick={(e) => { e.stopPropagation(); deleteRepository(selectedNS!, repo.name); }}
                    >
                      <Trash2 size={11} />
                    </button>
                  </div>
                {/if}
              </li>
            {/each}
          </ul>
        {/if}
      </section>

      <!-- Detail panel -->
      <main class="flex-1 min-w-0 overflow-y-auto bg-slate-100 dark:bg-warm-900 {selectedRepo || isRepoMode(mode) ? '' : 'hidden md:block'}">
        {#if isRepoMode(mode)}
          <!-- Inline repository create/edit form (replaces the old
               modal so the operator keeps the namespace + repo list
               in view while configuring). -->
          {@render repositoryFormPanel()}
        {:else if !selectedRepo}
          <div class="m-4 sm:m-6 h-[calc(100%-3rem)] leaf border-dashed flex flex-col items-center justify-center gap-2 px-6 text-center">
            <Package size={28} class="text-accent-500" />
            <p class="text-[14px] font-semibold text-slate-800 dark:text-warm-100">Pick a repository</p>
            <p class="text-[13px] text-slate-500 dark:text-warm-400 max-w-xs">Its endpoint, storage use and packages show up here.</p>
          </div>
        {:else}
          {@const repo = selectedRepo}
          {@const TypeIcon = iconFor(repo.type)}
          {@const KindIcon = kindIcon(repo.kind)}
          {@const endpoint = endpointURL(selectedNS ?? '', repo.name)}
          {@const setup = fillSnippet(protocol(repo.type).setup, snippetVars(endpoint, repo.name))}

          <div class="px-4 sm:px-6 py-5 max-w-5xl">
            <button type="button" class="btn btn-ghost btn-sm -ml-2 mb-2 md:hidden" onclick={() => (selectedRepo = null)}>
              <ChevronRight size={14} class="rotate-180" /> Repositories
            </button>
            <!-- Repo header -->
            <div class="flex flex-wrap items-center gap-2 mb-3">
              <TypeIcon size={22} class="text-accent-600 dark:text-accent-300" />
              <h2 class="text-[22px] font-bold tracking-tight mr-1">{repo.name}</h2>
              <span class="tag">
                <KindIcon size={11} />
                {repo.kind}
              </span>
              <span class="tag">
                {repo.type}
              </span>
              {#if (repo.kind === 'remote' || repo.kind === 'virtual') && canAdmin}
                <button
                  class="btn btn-secondary btn-sm ml-auto"
                  disabled={purgeRunning || repo.kind === 'virtual'}
                  onclick={() => runCachePurge(selectedNS ?? '', repo)}
                  title={repo.kind === 'virtual'
                    ? 'Virtual repos delegate to their members — purge each member individually'
                    : 'Drop cached upstream responses so the next read re-fetches'}
                >
                  {#if purgeRunning}
                    <Loader2 size={12} class="animate-spin" />
                  {:else}
                    <RotateCw size={12} />
                  {/if}
                  Refresh cache
                </button>
              {/if}
              <!--
                Docker Local cleanup moved out of the top-bar action
                row and into the dedicated "Garbage" card below the
                Statistics panel. The card surfaces an estimate
                first so the operator sees what they're about to
                delete; this avoids a click that does nothing on a
                clean registry.
              -->
              {#if repo.type === 'docker' && repo.kind === 'local' && canAdmin}
                <!-- intentional spacer: ml-auto on the next sibling needs the row -->
                <span class="ml-auto"></span>
              {/if}
            </div>

            {#if repo.description}
              <p class="text-[14px] text-slate-600 dark:text-warm-300 mb-4 max-w-[65ch]">
                {repo.description}
              </p>
            {/if}

            <!-- Endpoint snippet -->
            <div class="mb-4 leaf flex items-center gap-3 pl-4 pr-2 py-2">
              <span class="label-caps text-[11px] text-slate-500 dark:text-warm-400 shrink-0">Endpoint</span>
              <code class="flex-1 min-w-0 text-[13px] font-mono truncate select-all">{endpoint}</code>
              <div class="contents">
                <button
                  class="btn btn-ghost btn-sm btn-icon"
                  title="Copy endpoint URL"
                  aria-label="Copy endpoint URL"
                  onclick={() => copyToClipboard(endpoint)}
                >
                  <Copy size={14} />
                </button>
              </div>
            </div>

            <!-- Client setup -->
            <details class="mb-4 leaf" open={!LEGACY_TYPES.includes(repo.type)}>
              <summary class="cursor-pointer px-4 py-2 label-caps text-[11px] text-slate-500 dark:text-warm-400 flex items-center gap-2">
                Client setup
                <button
                  class="btn btn-ghost btn-sm btn-icon ml-auto"
                  title="Copy setup snippet"
                  aria-label="Copy setup snippet"
                  onclick={(e) => { e.preventDefault(); copyToClipboard(setup); }}
                >
                  <Copy size={13} />
                </button>
              </summary>
              <pre class="px-4 pb-3 text-[12px] font-mono whitespace-pre-wrap break-all text-slate-700 dark:text-warm-200">{setup}</pre>
            </details>

            <!-- Per-kind metadata strip -->
            <div class="mb-4 leaf grid grid-cols-2 md:grid-cols-3 overflow-hidden meta">
              <div class="cell">
                <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Type / Kind</div>
                <div class="font-mono">{repo.type} · {repo.kind}</div>
              </div>
              {#if repo.kind === 'local'}
                <div class="cell">
                  <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Mount</div>
                  <div class="font-mono truncate">{repo.mount}</div>
                </div>
                <div class="cell">
                  <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Base path</div>
                  <div class="font-mono truncate">{repo.base_path || '/'}</div>
                </div>
                <div class="cell">
                  <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Push enabled</div>
                  <div class="font-mono">{repo.allow_push ? 'yes' : 'no'}</div>
                </div>
                {#if repo.type === 'docker'}
                  <div class="cell">
                    <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Immutable tags</div>
                    <div class="font-mono truncate">{repo.policy?.immutable_tags?.join(', ') || 'none'}</div>
                  </div>
                  <div class="cell">
                    <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">GC grace</div>
                    <div class="font-mono">{formatSeconds(effectiveGCMinAge(repo))}</div>
                  </div>
                  <div class="cell">
                    <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Stale uploads</div>
                    <div class="font-mono">{formatSeconds(effectiveAbandonedUploadMaxAge(repo))}</div>
                  </div>
                {/if}
              {:else if repo.kind === 'remote'}
                <div class="cell col-span-2">
                  <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Upstream</div>
                  <div class="font-mono truncate">{repo.url}</div>
                </div>
                <div class="cell">
                  <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Cache mount</div>
                  <div class="font-mono truncate">{repo.mount}</div>
                </div>
                <div class="cell">
                  <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Cache path</div>
                  <div class="font-mono truncate">{repo.base_path || '/'}</div>
                </div>
              {:else if repo.kind === 'virtual'}
                <div class="cell col-span-2">
                  <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Members (in lookup order)</div>
                  <div class="font-mono truncate">
                    {repo.members?.join(', ') ?? '(none)'}
                  </div>
                </div>
              {/if}
            </div>

            <!-- Statistics card. Hidden for Virtual repos (no
                 backing store of their own — they delegate to
                 members). Re-fetched on each repo selection; the
                 server walks storage live so we don't poll. -->
            {#if repo.kind !== 'virtual'}
              <div class="mb-4 leaf p-3">
                <div class="flex items-center justify-between mb-2">
                  <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400">Statistics</div>
                  <button
                    class="btn btn-ghost btn-sm !h-6"
                    title="Re-fetch counts"
                    disabled={statsLoading}
                    onclick={() => selectedNS && loadStats(selectedNS, repo)}
                  >
                    {#if statsLoading}
                      <Loader2 size={12} class="animate-spin" />
                    {:else}
                      <RotateCw size={12} />
                    {/if}
                    Recount
                  </button>
                </div>
                {#if statsLoading && !stats}
                  <div class="text-[13px] text-slate-500 dark:text-warm-400">Loading…</div>
                {:else if !stats}
                  <div class="text-[13px] text-slate-500 dark:text-warm-400">Statistics unavailable.</div>
                {:else}
                  <div class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-2 text-[13px]">
                    {#if repo.type === 'go'}
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Modules</div>
                        <div class="font-mono">{stats.module_count ?? 0}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Versions</div>
                        <div class="font-mono">{stats.version_count ?? 0}</div>
                      </div>
                    {:else if repo.type === 'npm'}
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Packages</div>
                        <div class="font-mono">{stats.package_count ?? 0}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Versions</div>
                        <div class="font-mono">{stats.version_count ?? 0}</div>
                      </div>
                    {:else if repo.type === 'maven'}
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Artifacts</div>
                        <div class="font-mono">{stats.package_count ?? 0}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Versions</div>
                        <div class="font-mono">{stats.version_count ?? 0}</div>
                      </div>
                    {:else if repo.type === 'pypi'}
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Packages</div>
                        <div class="font-mono">{stats.package_count ?? 0}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Versions</div>
                        <div class="font-mono">{stats.version_count ?? 0}</div>
                      </div>
                    {:else if repo.type === 'cargo'}
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Crates</div>
                        <div class="font-mono">{stats.package_count ?? 0}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Versions</div>
                        <div class="font-mono">{stats.version_count ?? 0}</div>
                      </div>
                    {:else if !LEGACY_TYPES.includes(repo.type)}
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Packages</div>
                        <div class="font-mono">{stats.package_count ?? 0}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Versions</div>
                        <div class="font-mono">{stats.version_count ?? 0}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Files</div>
                        <div class="font-mono">{stats.blob_count ?? 0}</div>
                      </div>
                    {:else if repo.type === 'docker'}
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Repos</div>
                        <div class="font-mono">{stats.repository_count ?? 0}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Tags</div>
                        <div class="font-mono">{stats.tag_count ?? 0}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Manifests</div>
                        <div class="font-mono">{stats.manifest_count ?? 0}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Blobs</div>
                        <div class="font-mono">{stats.blob_count ?? 0}</div>
                      </div>
                    {/if}
                    <div>
                      <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Total size</div>
                      <div class="font-mono">{humanBytes(stats.total_bytes ?? 0)}</div>
                    </div>
                  </div>
                {/if}
              </div>
            {/if}

            <!-- Garbage card. Docker Local only — the cheap
                 delete-time cascade in the registry handler
                 already drops manifests + sidecars + orphan
                 referrer indexes anytime an image is deleted by
                 digest. Layer / config blobs are shareable, so
                 those wait for an explicit cleanup pass.
                 Operators trigger that pass here; the estimate
                 row tells them whether it's worth the click. -->
            {#if repo.type === 'docker' && repo.kind === 'local' && canAdmin}
              <div class="mb-4 leaf p-3">
                <div class="flex items-center justify-between mb-2">
                  <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400">Garbage</div>
                  <div class="flex items-center gap-2">
                    <button
                      class="text-[11px] flex items-center gap-1 px-1.5 py-0.5 rounded hover:bg-warm-100 dark:hover:bg-warm-800 disabled:opacity-50"
                      title="Dry-run mark-and-sweep: estimate reclaimable garbage without deleting anything"
                      disabled={gcEstimateLoading || gcRunning}
                      onclick={() => selectedNS && refreshGCEstimate(selectedNS, repo.name)}
                    >
                      {#if gcEstimateLoading}
                        <Loader2 size={10} class="animate-spin" />
                      {:else}
                        <RotateCw size={10} />
                      {/if}
                      estimate
                    </button>
                    <button
                      class="text-[11px] flex items-center gap-1 px-1.5 py-0.5 rounded border border-warm-300 dark:border-warm-700 hover:bg-warm-100 dark:hover:bg-warm-800 disabled:opacity-50"
                      title={`Run cleanup now: deletes unreferenced blobs, manifests and abandoned upload tmp files. Items modified in the last ${formatSeconds(effectiveGCMinAge(repo))} are protected.`}
                      disabled={gcRunning || gcEstimateLoading}
                      onclick={() => runDockerGC(selectedNS ?? '', repo.name)}
                    >
                      {#if gcRunning}
                        <Loader2 size={10} class="animate-spin" />
                      {:else}
                        <Trash2 size={10} />
                      {/if}
                      clean up
                    </button>
                  </div>
                </div>
                <div class="mb-2 text-[11px] text-slate-500 dark:text-warm-400">
                  Policy defaults: GC grace {formatSeconds(effectiveGCMinAge(repo))}, stale uploads {formatSeconds(effectiveAbandonedUploadMaxAge(repo))}.
                </div>
                {#if gcEstimateLoading && !gcEstimate}
                  <div class="text-[13px] text-slate-500 dark:text-warm-400">Calculating…</div>
                {:else if !gcEstimate || gcEstimateRepoKey !== `${selectedNS}/${repo.name}`}
                  <div class="text-[13px] text-slate-500 dark:text-warm-400">
                    Click <span class="font-medium">estimate</span> to see how much storage can be reclaimed.
                  </div>
                {:else}
                  {@const totalReclaimable = gcEstimate.swept_bytes + gcEstimate.abandoned_uploads_bytes}
                  {@const nothingToDo = gcEstimate.swept_blobs === 0
                    && gcEstimate.swept_manifests === 0
                    && gcEstimate.abandoned_uploads_removed === 0}
                  {#if nothingToDo}
                    <div class="text-[13px] text-slate-500 dark:text-warm-400">
                      Nothing reclaimable — registry is clean.
                    </div>
                  {:else}
                    <div class="grid grid-cols-2 sm:grid-cols-4 gap-2 text-[13px]">
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Reclaimable</div>
                        <div class="font-mono">{humanBytes(totalReclaimable)}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Blobs</div>
                        <div class="font-mono">{gcEstimate.swept_blobs}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Manifests</div>
                        <div class="font-mono">{gcEstimate.swept_manifests}</div>
                      </div>
                      <div>
                        <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Stale uploads</div>
                        <div class="font-mono">{gcEstimate.abandoned_uploads_removed}</div>
                      </div>
                    </div>
                    {#if gcEstimate.skipped_young > 0}
                      <div class="mt-2 text-[11px] text-slate-500 dark:text-warm-400">
                        {gcEstimate.skipped_young} item{gcEstimate.skipped_young === 1 ? '' : 's'} protected by grace window (modified &lt; {formatSeconds(effectiveGCMinAge(repo))} ago).
                      </div>
                    {/if}
                  {/if}
                {/if}
              </div>
            {/if}

            <!-- Browser. Each protocol renders its cached/local package
                 index with the same click-through detail interaction. -->
            <RepoOperations
              namespace={selectedNS ?? ''}
              {repo}
              siblings={(namespaces.find((n) => n.name === selectedNS)?.repositories ?? []).filter((r) => r.type === repo.type && r.kind === 'local' && r.name !== repo.name)}
              {canAdmin}
              {canDelete}
              onchanged={refreshAfterArtifactDelete}
            />

            {#if !LEGACY_TYPES.includes(repo.type)}
              <div class="leaf">
                <div class="px-3 py-2 border-b border-slate-200 dark:border-warm-700 flex items-center gap-2">
                  <FolderTree size={14} class="text-accent-500" />
                  <span class="text-[14px] font-semibold">Packages</span>
                  {#if entriesLoading}
                    <Loader2 size={12} class="animate-spin text-slate-500 dark:text-warm-400" />
                  {:else}
                    <span class="text-[11px] text-slate-500 dark:text-warm-400">({genericEntries.length})</span>
                  {/if}
                  {#if genericEntries.length > 0}
                    <div class="ml-auto flex items-center gap-1">
                      <Search size={10} class="text-slate-500 dark:text-warm-400" />
                      <input
                        type="text"
                        placeholder="Filter…"
                        bind:value={entryFilter}
                        class="text-[12px] px-1.5 py-0.5 w-32 bg-warm-50 dark:bg-warm-800 border border-warm-200 dark:border-warm-700 rounded focus:border-accent-500 outline-none"
                      />
                    </div>
                  {/if}
                </div>
                {#if genericEntries.length === 0 && !entriesLoading}
                  <div class="px-3 py-4 text-[13px] text-slate-500 dark:text-warm-400 text-center">
                    {#if repo.kind === 'local'}
                      Nothing published yet — see <span class="font-medium">Client setup</span> above.
                    {:else if repo.kind === 'remote'}
                      Nothing in cache yet — the first client request will populate this list.
                    {:else}
                      No member repository has packages yet.
                    {/if}
                  </div>
                {:else}
                  {@const GenIcon = iconFor(repo.type)}
                  <ul class="text-[14px]">
                    {#each genericEntries.filter((p) => matchFilter(p.name, entryFilter)) as p (p.name)}
                      {@const vs = p.versions ?? []}
                      <li class="border-b border-slate-200 dark:border-warm-700 last:border-b-0">
                        <button
                          class="w-full text-left px-3 py-2 hover:bg-slate-100 dark:hover:bg-warm-800 flex items-center gap-2"
                          onclick={() => openDetail(p.name, repo.type)}
                          title="View details"
                        >
                          <GenIcon size={14} class="text-slate-500 dark:text-warm-400" />
                          <span class="font-mono text-[13px] truncate">{p.name}</span>
                          {#if vs.length > 0}
                            <span class="text-[11px] text-accent-500 font-mono shrink-0">@{vs[vs.length - 1]}</span>
                          {/if}
                          <span class="ml-auto text-[11px] text-slate-500 dark:text-warm-400 shrink-0">
                            {vs.length} version{vs.length === 1 ? '' : 's'}
                          </span>
                          <ChevronRight size={12} class="text-slate-400 dark:text-warm-500 shrink-0" />
                        </button>
                      </li>
                    {/each}
                  </ul>
                {/if}
              </div>
            {:else if repo.type === 'go' || repo.type === 'npm' || repo.type === 'docker' || repo.type === 'helm' || repo.type === 'maven' || repo.type === 'pypi' || repo.type === 'cargo'}
              {@const isGo = repo.type === 'go'}
              {@const isNpm = repo.type === 'npm'}
              {@const isDocker = repo.type === 'docker'}
              {@const isHelm = repo.type === 'helm'}
              {@const isMaven = repo.type === 'maven'}
              {@const isPyPI = repo.type === 'pypi'}
              {@const isCargo = repo.type === 'cargo'}
              {@const entryLabel = isGo ? 'Modules' : isNpm ? 'Packages' : isDocker ? 'Repositories' : isHelm ? 'Charts' : isMaven ? 'Artifacts' : isPyPI ? 'Packages' : 'Crates'}
              {@const entryCount = isGo ? modules.length : isNpm ? packages.length : isDocker ? images.length : isHelm ? charts.length : isMaven ? mavenArtifacts.length : isPyPI ? pypiPackages.length : cargoCrates.length}
              <div class="leaf">
                <div class="px-3 py-2 border-b border-slate-200 dark:border-warm-700 flex items-center gap-2">
                  <FolderTree size={14} class="text-accent-500" />
                  <span class="text-[14px] font-semibold">{entryLabel}</span>
                  {#if entriesLoading}
                    <Loader2 size={12} class="animate-spin text-slate-500 dark:text-warm-400" />
                  {:else if repo.kind !== 'virtual'}
                    <span class="text-[11px] text-slate-500 dark:text-warm-400">({entryCount})</span>
                  {/if}
                  {#if repo.kind !== 'virtual' && entryCount > 0}
                    <div class="ml-auto flex items-center gap-1">
                      <Search size={10} class="text-slate-500 dark:text-warm-400" />
                      <input
                        type="text"
                        placeholder="Filter…"
                        bind:value={entryFilter}
                        class="text-[12px] px-1.5 py-0.5 w-32 bg-warm-50 dark:bg-warm-800 border border-warm-200 dark:border-warm-700 rounded focus:border-accent-500 outline-none"
                      />
                    </div>
                  {/if}
                </div>
                {#if repo.kind === 'virtual'}
                  <div class="px-3 py-2 text-[13px] text-slate-500 dark:text-warm-400">
                    Virtual repositories aggregate at request time; entry
                    listings live on the underlying member repositories.
                  </div>
                {:else if entryCount === 0 && !entriesLoading}
                  <div class="px-3 py-4 text-[13px] text-slate-500 dark:text-warm-400 text-center">
                    {#if repo.kind === 'local'}
                      {isGo ? 'No modules' : isNpm ? 'No packages' : isDocker ? 'No images' : isHelm ? 'No charts' : isMaven ? 'No artifacts' : isPyPI ? 'No packages' : 'No crates'} uploaded yet.
                      {#if isGo && repo.allow_push}
                        <div class="mt-2 text-[12px] font-mono text-left bg-slate-100 dark:bg-warm-950 p-2.5 rounded-[3px]">
                          # upload via curl{'\n'}
                          curl -XPUT -H "Authorization: Bearer $TOKEN" \{'\n'}
                          {'  '}-H "Content-Type: application/json" \{'\n'}
                          {'  '}--data '{`{"Version":"v1.0.0"}`}' \{'\n'}
                          {'  '}{endpoint}/&lt;module&gt;/@v/v1.0.0.info
                        </div>
                      {:else if isNpm && repo.allow_push}
                        <div class="mt-2 text-[12px] font-mono text-left bg-slate-100 dark:bg-warm-950 p-2.5 rounded-[3px]">
                          # publish via npm{'\n'}
                          npm publish --registry={endpoint}/
                        </div>
                      {:else if isDocker && repo.allow_push}
                        <div class="mt-2 text-[12px] font-mono text-left bg-slate-100 dark:bg-warm-950 p-2.5 rounded-[3px]">
                          # push via docker{'\n'}
                          docker tag &lt;img&gt; {endpoint.replace(/^https?:\/\//, '')}/v2/&lt;img&gt;:&lt;tag&gt;{'\n'}
                          docker push {endpoint.replace(/^https?:\/\//, '')}/v2/&lt;img&gt;:&lt;tag&gt;
                        </div>
                      {:else if isHelm && repo.allow_push}
                        <div class="mt-2 text-[12px] font-mono text-left bg-slate-100 dark:bg-warm-950 p-2.5 rounded-[3px]">
                          # publish via curl (ChartMuseum-compatible){'\n'}
                          curl -XPOST -H "Authorization: Bearer $TOKEN" \{'\n'}
                          {'  '}--data-binary @mychart-1.0.0.tgz \{'\n'}
                          {'  '}{endpoint}/api/charts
                        </div>
                      {:else if isMaven && repo.allow_push}
                        <div class="mt-2 text-[12px] font-mono text-left bg-slate-100 dark:bg-warm-950 p-2.5 rounded-[3px]">
                          # deploy via Maven settings.xml / pom.xml repository URL{`\n`}
                          {endpoint}/com/example/app/1.0.0/app-1.0.0.jar
                        </div>
                      {:else if isPyPI && repo.allow_push}
                        <div class="mt-2 text-[12px] font-mono text-left bg-slate-100 dark:bg-warm-950 p-2.5 rounded-[3px]">
                          # publish via twine{`\n`}
                          twine upload --repository-url {endpoint}/ dist/*
                        </div>
                      {:else if isCargo && repo.allow_push}
                        <div class="mt-2 text-[12px] font-mono text-left bg-slate-100 dark:bg-warm-950 p-2.5 rounded-[3px]">
                          # seed a crate archive via HTTP PUT{`\n`}
                          curl -XPUT --data-binary @crate.crate {endpoint}/api/v1/crates/&lt;crate&gt;/&lt;version&gt;/download
                        </div>
                      {/if}
                    {:else}
                      Nothing in cache yet — the first client request will
                      populate this list.
                    {/if}
                  </div>
                {:else if isGo}
                  <ul class="text-[14px]">
                    {#each modules.filter((m) => matchFilter(m.module, entryFilter)) as m (m.module)}
                      <li class="border-b border-slate-200 dark:border-warm-700 last:border-b-0">
                        <button
                          class="w-full text-left px-3 py-2 hover:bg-slate-100 dark:hover:bg-warm-800 flex items-center gap-2"
                          onclick={() => openDetail(m.module, 'go')}
                          title="View details"
                        >
                          <FileBox size={14} class="text-slate-500 dark:text-warm-400" />
                          <span class="font-mono text-[13px]">{m.module}</span>
                          <span class="ml-auto text-[11px] text-slate-500 dark:text-warm-400">
                            {m.versions.length} version{m.versions.length === 1 ? '' : 's'}
                          </span>
                          <ChevronRight size={12} class="text-slate-400 dark:text-warm-500" />
                        </button>
                      </li>
                    {/each}
                  </ul>
                {:else if isDocker}
                  <ul class="text-[14px]">
                    {#each images.filter((i) => matchFilter(i.name, entryFilter)) as img (img.name)}
                      {@const hasArtifacts = img.tags.some((t) => t.artifact_type)}
                      <li class="border-b border-slate-200 dark:border-warm-700 last:border-b-0">
                        <button
                          class="w-full text-left px-3 py-2 hover:bg-slate-100 dark:hover:bg-warm-800 flex items-center gap-2"
                          onclick={() => openDetail(img.name, 'docker')}
                          title="View details"
                        >
                          <Container size={14} class="text-slate-500 dark:text-warm-400" />
                          <span class="font-mono text-[13px]">{img.name}</span>
                          {#if hasArtifacts}
                            <span class="text-[11px] uppercase px-1 py-0.5 rounded bg-accent-500/10 text-accent-500 border border-accent-500/30">
                              OCI artifact
                            </span>
                          {/if}
                          <span class="ml-auto text-[11px] text-slate-500 dark:text-warm-400">
                            {img.tags.length} tag{img.tags.length === 1 ? '' : 's'}
                          </span>
                          <ChevronRight size={12} class="text-slate-400 dark:text-warm-500" />
                        </button>
                      </li>
                    {/each}
                  </ul>
                {:else if isNpm}
                  <ul class="text-[14px]">
                    {#each packages.filter((p) => matchFilter(p.name, entryFilter)) as p (p.name)}
                      <li class="border-b border-slate-200 dark:border-warm-700 last:border-b-0">
                        <button
                          class="w-full text-left px-3 py-2 hover:bg-slate-100 dark:hover:bg-warm-800 flex items-center gap-2"
                          onclick={() => openDetail(p.name, 'npm')}
                          title="View details"
                        >
                          <Package size={14} class="text-slate-500 dark:text-warm-400" />
                          <span class="font-mono text-[13px]">{p.name}</span>
                          {#if p.dist_tags?.latest}
                            <span class="text-[11px] text-accent-500 font-mono">@{p.dist_tags.latest}</span>
                          {/if}
                          <span class="ml-auto text-[11px] text-slate-500 dark:text-warm-400">
                            {p.versions.length} version{p.versions.length === 1 ? '' : 's'}
                          </span>
                          <ChevronRight size={12} class="text-slate-400 dark:text-warm-500" />
                        </button>
                      </li>
                    {/each}
                  </ul>
                {:else if isHelm}
                  <ul class="text-[14px]">
                    {#each charts.filter((ch) => matchFilter(ch.name, entryFilter)) as ch (ch.name)}
                      <li class="border-b border-slate-200 dark:border-warm-700 last:border-b-0">
                        <button
                          class="w-full text-left px-3 py-2 hover:bg-slate-100 dark:hover:bg-warm-800 flex items-center gap-2"
                          onclick={() => openDetail(ch.name, 'helm')}
                          title="View details"
                        >
                          <Anchor size={14} class="text-slate-500 dark:text-warm-400" />
                          <span class="font-mono text-[13px]">{ch.name}</span>
                          <span class="ml-auto text-[11px] text-slate-500 dark:text-warm-400">
                            {ch.versions.length} version{ch.versions.length === 1 ? '' : 's'}
                          </span>
                          <ChevronRight size={12} class="text-slate-400 dark:text-warm-500" />
                        </button>
                      </li>
                    {/each}
                  </ul>
                {:else if isMaven}
                  <ul class="text-[14px]">
                    {#each mavenArtifacts.filter((a) => matchFilter(`${a.group_id}:${a.artifact_id}`, entryFilter)) as art (`${art.group_id}:${art.artifact_id}`)}
                      <li class="border-b border-slate-200 dark:border-warm-700 last:border-b-0">
                        <button
                          class="w-full text-left px-3 py-2 hover:bg-slate-100 dark:hover:bg-warm-800 flex items-center gap-2"
                          onclick={() => openDetail(`${art.group_id}:${art.artifact_id}`, 'maven')}
                          title="View details"
                        >
                          <Package size={14} class="text-slate-500 dark:text-warm-400" />
                          <span class="font-mono text-[13px]">{art.group_id}:{art.artifact_id}</span>
                          <span class="ml-auto text-[11px] text-slate-500 dark:text-warm-400">
                            {art.versions.length} version{art.versions.length === 1 ? '' : 's'}
                          </span>
                          <ChevronRight size={12} class="text-slate-400 dark:text-warm-500" />
                        </button>
                      </li>
                    {/each}
                  </ul>
                {:else if isPyPI}
                  <ul class="text-[14px]">
                    {#each pypiPackages.filter((p) => matchFilter(p.name, entryFilter)) as p (p.name)}
                      <li class="border-b border-slate-200 dark:border-warm-700 last:border-b-0">
                        <button
                          class="w-full text-left px-3 py-2 hover:bg-slate-100 dark:hover:bg-warm-800 flex items-center gap-2"
                          onclick={() => openDetail(p.name, 'pypi')}
                          title="View details"
                        >
                          <Package size={14} class="text-slate-500 dark:text-warm-400" />
                          <span class="font-mono text-[13px]">{p.name}</span>
                          <span class="ml-auto text-[11px] text-slate-500 dark:text-warm-400">
                            {p.versions.length} version{p.versions.length === 1 ? '' : 's'}
                          </span>
                          <ChevronRight size={12} class="text-slate-400 dark:text-warm-500" />
                        </button>
                      </li>
                    {/each}
                  </ul>
                {:else if isCargo}
                  <ul class="text-[14px]">
                    {#each cargoCrates.filter((c) => matchFilter(c.name, entryFilter)) as cr (cr.name)}
                      <li class="border-b border-slate-200 dark:border-warm-700 last:border-b-0">
                        <button
                          class="w-full text-left px-3 py-2 hover:bg-slate-100 dark:hover:bg-warm-800 flex items-center gap-2"
                          onclick={() => openDetail(cr.name, 'cargo')}
                          title="View details"
                        >
                          <Package size={14} class="text-slate-500 dark:text-warm-400" />
                          <span class="font-mono text-[13px]">{cr.name}</span>
                          <span class="ml-auto text-[11px] text-slate-500 dark:text-warm-400">
                            {cr.versions.length} version{cr.versions.length === 1 ? '' : 's'}
                          </span>
                          <ChevronRight size={12} class="text-slate-400 dark:text-warm-500" />
                        </button>
                      </li>
                    {/each}
                  </ul>
                {/if}
              </div>
            {:else}
              <div class="leaf px-3 py-4 text-[13px] text-slate-500 dark:text-warm-400 text-center">
                The browser for {repo.type} registries is coming in a follow-up
                phase. The endpoint above is already live.
              </div>
            {/if}
          </div>
        {/if}
      </main>
    </div>
  {/if}

  <!-- ─── Admin modal (new/edit namespace) ──────────────────────────
       Repository create/edit moved inline into the detail column
       (see repositoryFormPanel snippet below); only namespace CRUD
       still uses the modal. -->
  <Modal open={isNamespaceMode(mode)} onClose={cancelModal} size="sm">
    {#snippet header()}
      {#if mode}
        <h2 class="text-base font-semibold">
          {#if mode.kind === 'new-namespace'}New namespace
          {:else}Edit namespace
          {/if}
        </h2>
        <button class="p-1 rounded hover:bg-warm-100 dark:hover:bg-warm-800" onclick={cancelModal} title="Cancel">
          <X size={16} />
        </button>
      {/if}
    {/snippet}

    {#if isNamespaceMode(mode)}
        <div class="p-4 space-y-3 text-[14px]">
            <!-- ── Namespace form ── -->
            <label class="block">
              <span class="field-label block mb-1.5">
                Name <span class="text-vermilion-500">*</span>
              </span>
              <input
                type="text"
                class="input font-mono"
                bind:value={nsDraft.name}
                placeholder="my-team"
                pattern="[a-z0-9_-]+"
              />
              <span class="field-hint block mt-1">
                Lowercase alphanumerics, hyphen and underscore. Becomes the URL path segment.
              </span>
            </label>

            <label class="block">
              <span class="field-label block mb-1.5">Description</span>
              <input
                type="text"
                class="input"
                bind:value={nsDraft.description}
                placeholder="Optional"
              />
            </label>
        </div>
    {/if}

    {#snippet footer()}
      <button
        class="px-3 py-1 text-[13px] rounded border border-warm-300 dark:border-warm-700 hover:bg-warm-100 dark:hover:bg-warm-800"
        onclick={cancelModal}
        disabled={saving}
      >
        Cancel
      </button>
      <button
        class="px-3 py-1 text-[13px] rounded bg-accent-500 hover:bg-accent-600 text-white disabled:opacity-50 flex items-center gap-1"
        onclick={saveNamespace}
        disabled={saving}
      >
        {#if saving}<Loader2 size={12} class="animate-spin" />{/if}
        Save
      </button>
    {/snippet}
  </Modal>

  <!-- ─── Inline repository create/edit form ──────────────────────────
       Rendered in the detail column (via {@render repositoryFormPanel()})
       instead of a modal. All draft state + saveRepository live in this
       component, so the snippet just lays out the fields. -->
  {#snippet repositoryFormPanel()}
    <div class="p-4 max-w-3xl">
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-base font-semibold">
          {#if mode?.kind === 'new-repository'}New repository in {modeNamespace(mode)}
          {:else}Edit repository in {modeNamespace(mode)}
          {/if}
        </h2>
        <button class="p-1 rounded hover:bg-warm-100 dark:hover:bg-warm-800" onclick={cancelModal} title="Cancel">
          <X size={16} />
        </button>
      </div>
      <div class="rounded-lg border border-slate-200 dark:border-warm-700 bg-white dark:bg-warm-900 p-4 space-y-3 text-[14px]">
            <!-- ── Repository form ── -->
            <label class="block">
              <span class="field-label block mb-1.5">
                Name <span class="text-vermilion-500">*</span>
              </span>
              <input
                type="text"
                class="input font-mono"
                bind:value={repoDraft.name}
                placeholder="proxy-cache"
                pattern="[a-z0-9_-]+"
              />
            </label>

            <label class="block">
              <span class="field-label block mb-1.5">Description</span>
              <input
                type="text"
                class="input"
                bind:value={repoDraft.description}
                placeholder="Optional"
              />
            </label>

            <div class="grid grid-cols-2 gap-3">
              <label class="block">
                <span class="field-label block mb-1.5">
                  Type <span class="text-vermilion-500">*</span>
                </span>
                <select
                  class="input"
                  bind:value={repoDraft.type}
                >
                  {#each PROTOCOLS as p (p.type)}
                    <option value={p.type}>{p.label}</option>
                  {/each}
                </select>
              </label>

              <label class="block">
                <span class="field-label block mb-1.5">
                  Kind <span class="text-vermilion-500">*</span>
                </span>
                <select
                  class="input"
                  bind:value={repoDraft.kind}
                >
                  <option value="local">Local — stored on a raw mount</option>
                  <option value="remote">Remote — proxy + cache an upstream</option>
                  <option value="virtual">Virtual — aggregate sibling repos</option>
                </select>
              </label>
            </div>

            {#if repoDraft.kind === 'local'}
              <!-- ── Local: mount + base path ── -->
              <div class="rounded border border-slate-200 dark:border-warm-700 p-3 bg-warm-50/50 dark:bg-warm-950/30 space-y-3">
                <label class="block">
                  <span class="field-label block mb-1.5">
                    Storage mount <span class="text-vermilion-500">*</span>
                  </span>
                  {#if rawMounts.length === 0}
                    <div class="text-[12px] text-amber-700 dark:text-amber-400">
                      No raw mounts configured. Add one under Settings → Raw mounts first.
                    </div>
                  {:else}
                    <select
                      class="input font-mono"
                      bind:value={repoDraft.mount}
                    >
                      <option value="">— select a mount —</option>
                      {#each rawMounts as m (m.prefix)}
                        <option value={m.prefix}>{m.prefix}</option>
                      {/each}
                    </select>
                  {/if}
                </label>

                <label class="block">
                  <span class="field-label block mb-1.5">
                    Base path <span class="text-vermilion-500">*</span>
                  </span>
                  <input
                    type="text"
                    class="input font-mono"
                    bind:value={repoDraft.base_path}
                    placeholder={defaultBasePath(repoDraft.type)}
                  />
                  <span class="field-hint block mt-1">
                    Path inside the mount where artifacts will be stored.
                  </span>
                </label>

                <label class="flex items-center gap-2 cursor-pointer">
                  <input
                    type="checkbox"
                    class="rounded border-warm-400 dark:border-warm-600"
                    checked={repoDraft.allow_push !== false}
                    onchange={(e) => repoDraft.allow_push = (e.currentTarget as HTMLInputElement).checked}
                  />
                  <span class="text-[13px]">Allow publish / push (otherwise read-only)</span>
                </label>

                {#if repoDraft.type === 'docker'}
                  <div class="rounded border border-slate-200 dark:border-warm-700 p-3 bg-white dark:bg-warm-900 space-y-3">
                    <div>
                      <div class="text-[13px] font-medium text-warm-700 dark:text-warm-300">Docker policy + retention</div>
                      <div class="text-[11px] text-slate-500 dark:text-warm-400">
                        Enforced by the local Docker handler. Remote and virtual repos keep policy on their member repos.
                      </div>
                    </div>

                    <label class="block">
                      <span class="field-label block mb-1.5">
                        Immutable tag patterns
                      </span>
                      <input
                        type="text"
                        class="input font-mono"
                        bind:value={immutableTagsText}
                        placeholder="prod, release-*, v*"
                      />
                      <span class="block mt-1 text-[11px] text-slate-500 dark:text-warm-400 leading-relaxed">
                        Comma-separated shell-style tag patterns. Matching tags can be created once,
                        then cannot be moved to another digest or deleted. Do not include image paths.
                      </span>
                    </label>

                    <div class="grid grid-cols-2 gap-3">
                      <label class="block">
                        <span class="field-label block mb-1.5">
                          GC grace seconds
                        </span>
                        <input
                          type="number"
                          min="0"
                          step="1"
                          class="input font-mono"
                          bind:value={gcMinAgeSeconds}
                          placeholder="3600"
                        />
                        <span class="field-hint block mt-1">0 = default 1h</span>
                      </label>

                      <label class="block">
                        <span class="field-label block mb-1.5">
                          Stale upload seconds
                        </span>
                        <input
                          type="number"
                          min="0"
                          step="1"
                          class="input font-mono"
                          bind:value={abandonedUploadMaxAgeSeconds}
                          placeholder="86400"
                        />
                        <span class="field-hint block mt-1">0 = default 24h</span>
                      </label>
                    </div>
                  </div>
                {/if}
              </div>
            {:else if repoDraft.kind === 'remote'}
              <!-- ── Remote: upstream URL + cache storage + optional auth ── -->
              <div class="rounded border border-slate-200 dark:border-warm-700 p-3 bg-warm-50/50 dark:bg-warm-950/30 space-y-3">
                <label class="block">
                  <span class="field-label block mb-1.5">
                    Upstream URL <span class="text-vermilion-500">*</span>
                  </span>
                  <input
                    type="url"
                    class="input font-mono"
                    bind:value={repoDraft.url}
                    placeholder={defaultUpstreamURL(repoDraft.type)}
                  />
                </label>

                <div class="grid grid-cols-2 gap-3">
                  <label class="block">
                    <span class="field-label block mb-1.5">
                      Cache mount <span class="text-vermilion-500">*</span>
                    </span>
                    {#if rawMounts.length === 0}
                      <div class="text-[12px] text-amber-700 dark:text-amber-400">
                        No raw mounts configured. Add one under Settings → Raw mounts first.
                      </div>
                    {:else}
                      <select
                        class="input font-mono"
                        bind:value={repoDraft.mount}
                      >
                        <option value="">— select a mount —</option>
                        {#each rawMounts as m (m.prefix)}
                          <option value={m.prefix}>{m.prefix}</option>
                        {/each}
                      </select>
                    {/if}
                  </label>

                  <label class="block">
                    <span class="field-label block mb-1.5">
                      Cache base path <span class="text-vermilion-500">*</span>
                    </span>
                    <input
                      type="text"
                      class="input font-mono"
                      bind:value={repoDraft.base_path}
                      placeholder={defaultCachePath(repoDraft.type)}
                    />
                  </label>
                </div>

                <span class="block -mt-1 text-[11px] text-slate-500 dark:text-warm-400 leading-relaxed">
                  Pulled artifacts are stored in this raw mount path and served from cache
                  on later reads. Use a dedicated cache path per remote repository.
                </span>

                <!-- B8: connectivity probe button. Only enabled
                     when editing an existing repo (the probe uses
                     the persisted credentials, so the draft state
                     must already be saved). For new repos the
                     button is hidden — operators publish first,
                     then probe. -->
                {#if mode?.kind === 'edit-repository'}
                  <div class="flex items-center gap-2 text-[13px]">
                    <button
                      class="px-2 py-1 rounded border border-warm-300 dark:border-warm-700 hover:bg-warm-100 dark:hover:bg-warm-800 flex items-center gap-1 disabled:opacity-50"
                      onclick={runProbe}
                      disabled={probeRunning}
                    >
                      {#if probeRunning}<Loader2 size={10} class="animate-spin" />{:else}<RotateCw size={10} />{/if}
                      Test upstream
                    </button>
                    {#if probeResult}
                      {#if probeResult.ok}
                        <span class="text-green-600 dark:text-green-400">
                          ✓ {probeResult.status_code} · {probeResult.latency_ms} ms
                        </span>
                      {:else}
                        <span class="text-vermilion-500" title={probeResult.error}>
                          ✗ {probeResult.status_code || 'error'}
                        </span>
                      {/if}
                    {/if}
                  </div>
                  {#if probeResult && probeResult.body_preview}
                    <details class="text-[11px] text-slate-500 dark:text-warm-400">
                      <summary class="cursor-pointer">Response preview</summary>
                      <pre class="mt-1 font-mono whitespace-pre-wrap break-all">{probeResult.body_preview}</pre>
                    </details>
                  {/if}
                {/if}

                <label class="block">
                  <span class="field-label block mb-1.5">
                    Mutable cache TTL
                  </span>
                  <input
                    type="text"
                    class="input font-mono"
                    bind:value={repoDraft.mutable_ttl}
                    placeholder={repoDraft.type === 'docker' ? '1h' : '5m'}
                  />
                  <span class="block mt-1 text-[11px] text-slate-500 dark:text-warm-400 leading-relaxed">
                    {#if repoDraft.type === 'go'}
                      How long to cache <strong>mutable</strong> upstream responses — the version
                      list (<code class="font-mono">@v/list</code>) and latest pointer
                      (<code class="font-mono">@latest</code>). Within the TTL kutu serves the
                      cached copy without hitting <code class="font-mono">proxy.golang.org</code>;
                      after it, the next request triggers a refresh. Immutable files
                      (<code class="font-mono">.info</code>, <code class="font-mono">.mod</code>,
                      <code class="font-mono">.zip</code>) are cached forever regardless.
                      Default: <code class="font-mono">5m</code>. Set <code class="font-mono">0s</code>
                      to disable the cache (always hit upstream) or a longer duration
                      (<code class="font-mono">1h</code>, <code class="font-mono">24h</code>)
                      to reduce upstream traffic at the cost of slower version-list freshness.
                    {:else if repoDraft.type === 'npm'}
                      How long to cache <strong>mutable</strong> packument responses (the
                      package metadata document that lists versions and dist-tags). Tarballs
                      themselves are content-addressed and cached forever. Default:
                      <code class="font-mono">5m</code>. Set <code class="font-mono">0s</code>
                      to always re-fetch, or a longer duration to reduce upstream load.
                    {:else if repoDraft.type === 'maven'}
                      How long to cache <strong>mutable</strong> Maven metadata files
                      (<code class="font-mono">maven-metadata.xml</code>). Versioned JAR/POM
                      artifacts are cached forever after first fetch. Default:
                      <code class="font-mono">5m</code>.
                    {:else if repoDraft.type === 'pypi'}
                      How long to cache <strong>mutable</strong> PEP 503 simple index pages.
                      Distribution files are cached forever after first download. Default:
                      <code class="font-mono">5m</code>.
                    {:else if repoDraft.type === 'cargo'}
                      How long to cache <strong>mutable</strong> sparse-index files. Downloaded
                      <code class="font-mono">.crate</code> archives are cached forever after
                      first fetch. Default: <code class="font-mono">5m</code>.
                    {:else if repoDraft.type !== 'docker'}
                      {protocol(repoDraft.type).ttlHint} Default: <code class="font-mono">5m</code>.
                    {:else}
                      How long to cache <strong>floating</strong> Docker tags (see the field
                      below). Blob layers and manifests-by-digest are immutable and cached
                      forever; tag → digest lookups for non-floating tags (semver, dated)
                      are also cached forever. This TTL <em>only</em> applies to tags listed
                      as "floating". Default: <code class="font-mono">5m</code>.
                    {/if}
                    <br />
                    Format: Go duration string — <code class="font-mono">5m</code>,
                    <code class="font-mono">1h</code>, <code class="font-mono">24h</code>,
                    <code class="font-mono">0s</code>.
                  </span>
                </label>

                {#if repoDraft.type === 'docker'}
                  <label class="block">
                    <span class="field-label block mb-1.5">
                      Floating tags
                    </span>
                    <input
                      type="text"
                      class="input font-mono"
                      bind:value={floatingTagsText}
                      placeholder="latest, main, master, dev, develop, nightly, edge, stable, canary"
                    />
                    <span class="block mt-1 text-[11px] text-slate-500 dark:text-warm-400 leading-relaxed">
                      Comma-separated list of tag names treated as <strong>mutable</strong>.
                      kutu re-resolves these tags through upstream every TTL window. Tags
                      <strong>not</strong> in this list (e.g. <code class="font-mono">v1.2.3</code>,
                      <code class="font-mono">2024-05-19</code>) are cached forever after the
                      first successful resolve — once kutu has the digest, it never asks
                      upstream again, even after a registry restart.
                      <br />
                      Leave empty to use the default list shown as placeholder. Use a single
                      <code class="font-mono">*</code> to make every tag floating (matches the
                      pre-classification behaviour, useful for upstreams that overwrite semver
                      tags). Matching is case-insensitive.
                    </span>
                  </label>
                {/if}

                <label class="flex items-center gap-2 cursor-pointer">
                  <input
                    type="checkbox"
                    class="rounded border-warm-400 dark:border-warm-600"
                    checked={repoDraft.insecure_skip_verify === true}
                    onchange={(e) => repoDraft.insecure_skip_verify = (e.currentTarget as HTMLInputElement).checked}
                  />
                  <span class="text-[13px]">Skip TLS verify (self-signed upstream only)</span>
                </label>

                <label class="block">
                  <span class="field-label block mb-1.5">
                    Upstream auth (optional)
                  </span>
                  <select
                    class="input"
                    value={repoDraft.auth?.type ?? ''}
                    onchange={(e) => {
                      const v = (e.currentTarget as HTMLSelectElement).value;
                      if (!v) repoDraft.auth = undefined;
                      else repoDraft.auth = { ...(repoDraft.auth ?? {}), type: v as 'basic' | 'bearer' | 'header' };
                    }}
                  >
                    <option value="">None</option>
                    <option value="basic">HTTP basic (username + password)</option>
                    <option value="bearer">Bearer token</option>
                    <option value="header">Custom header</option>
                  </select>
                </label>

                {#if repoDraft.auth?.type === 'basic'}
                  <div class="grid grid-cols-2 gap-3">
                    <input
                      type="text"
                      class="input font-mono"
                      placeholder="username"
                      bind:value={repoDraft.auth.username}
                    />
                    <!--
                      Password field. Rendered as type=password by
                      default so casual screen-watching doesn't leak
                      the value; click 👁 to flip the input type to
                      text. The on-disk value is sealed via the
                      secret layer; the server transparently injects
                      plaintext back into the form on edit.
                    -->
                    <div class="relative">
                      <input
                        type={revealPassword ? 'text' : 'password'}
                        class="w-full px-2 py-1 pr-7 rounded border border-warm-300 dark:border-warm-700 bg-white dark:bg-warm-800 font-mono text-[13px]"
                        placeholder="password"
                        bind:value={repoDraft.auth.password}
                      />
                      <button
                        type="button"
                        class="absolute right-1 top-1/2 -translate-y-1/2 p-1 rounded hover:bg-warm-100 dark:hover:bg-warm-700"
                        onclick={() => (revealPassword = !revealPassword)}
                        title={revealPassword ? 'Hide password' : 'Show password'}
                      >
                        {#if revealPassword}<EyeOff size={11} />{:else}<Eye size={11} />{/if}
                      </button>
                    </div>
                  </div>
                {:else if repoDraft.auth?.type === 'bearer'}
                  <div class="relative">
                    <input
                      type={revealToken ? 'text' : 'password'}
                      class="w-full px-2 py-1 pr-7 rounded border border-warm-300 dark:border-warm-700 bg-white dark:bg-warm-800 font-mono text-[13px]"
                      placeholder="token"
                      bind:value={repoDraft.auth.token}
                    />
                    <button
                      type="button"
                      class="absolute right-1 top-1/2 -translate-y-1/2 p-1 rounded hover:bg-warm-100 dark:hover:bg-warm-700"
                      onclick={() => (revealToken = !revealToken)}
                      title={revealToken ? 'Hide token' : 'Show token'}
                    >
                      {#if revealToken}<EyeOff size={11} />{:else}<Eye size={11} />{/if}
                    </button>
                  </div>
                {:else if repoDraft.auth?.type === 'header'}
                  <div class="grid grid-cols-2 gap-3">
                    <input
                      type="text"
                      class="input font-mono"
                      placeholder="X-Header-Name"
                      bind:value={repoDraft.auth.header}
                    />
                    <div class="relative">
                      <input
                        type={revealHeaderValue ? 'text' : 'password'}
                        class="w-full px-2 py-1 pr-7 rounded border border-warm-300 dark:border-warm-700 bg-white dark:bg-warm-800 font-mono text-[13px]"
                        placeholder="value"
                        bind:value={repoDraft.auth.value}
                      />
                      <button
                        type="button"
                        class="absolute right-1 top-1/2 -translate-y-1/2 p-1 rounded hover:bg-warm-100 dark:hover:bg-warm-700"
                        onclick={() => (revealHeaderValue = !revealHeaderValue)}
                        title={revealHeaderValue ? 'Hide value' : 'Show value'}
                      >
                        {#if revealHeaderValue}<EyeOff size={11} />{:else}<Eye size={11} />{/if}
                      </button>
                    </div>
                  </div>
                {/if}

                <p class="text-[11px] text-slate-500 dark:text-warm-400 leading-relaxed">
                  Enter credentials directly. Secret values (password, token, header value) are
                  sealed with the at-rest encryption key before they are stored and are never
                  returned to non-admin API callers.
                </p>

                {#if protocol(repoDraft.type).prefixUpstreams}
                  <!-- ── Prefix-routed upstreams (go / npm / maven) ── -->
                  <div class="rounded border border-slate-200 dark:border-warm-700 p-3 bg-white dark:bg-warm-900 space-y-3">
                    <div class="flex items-start justify-between gap-2">
                      <div>
                        <div class="text-[13px] font-medium text-warm-700 dark:text-warm-300">Prefix-routed upstreams</div>
                        <div class="text-[11px] text-slate-500 dark:text-warm-400 leading-relaxed">
                          Route package names to different upstreams (e.g. <code class="font-mono">github.com/acme/</code>,
                          <code class="font-mono">@acme/</code>, <code class="font-mono">com/acme/</code>). The longest matching
                          prefix wins; everything else uses the upstream URL above.
                        </div>
                      </div>
                      <button
                        type="button"
                        class="shrink-0 flex items-center gap-1 text-[12px] px-2 py-1 rounded border border-warm-300 dark:border-warm-700 hover:bg-warm-100 dark:hover:bg-warm-800"
                        onclick={addUpstream}
                      >
                        <Plus size={11} /> Add upstream
                      </button>
                    </div>

                    {#each repoDraft.upstreams ?? [] as up, i (i)}
                      <div class="rounded border border-slate-200 dark:border-warm-700 p-2 space-y-2 bg-warm-50/50 dark:bg-warm-950/30">
                        <div class="flex items-center gap-2">
                          <input
                            type="text"
                            class="flex-1 px-2 py-1 rounded border border-warm-300 dark:border-warm-700 bg-white dark:bg-warm-800 font-mono text-[13px]"
                            placeholder="prefix, e.g. github.com/acme/"
                            bind:value={up.prefix}
                          />
                          <button
                            type="button"
                            class="p-1 rounded hover:bg-vermilion-100 dark:hover:bg-vermilion-900/40 text-vermilion-600 dark:text-vermilion-400"
                            title="Remove upstream"
                            onclick={() => removeUpstream(i)}
                          >
                            <Trash2 size={12} />
                          </button>
                        </div>

                        <input
                          type="url"
                          class="input font-mono"
                          placeholder="https://goproxy.internal"
                          bind:value={up.url}
                        />

                        <select
                          class="input"
                          value={up.auth?.type ?? ''}
                          onchange={(e) => setUpstreamAuthType(up, (e.currentTarget as HTMLSelectElement).value)}
                        >
                          <option value="">No auth</option>
                          <option value="basic">HTTP basic</option>
                          <option value="bearer">Bearer token</option>
                          <option value="header">Custom header</option>
                        </select>

                        {#if up.auth?.type === 'basic'}
                          <div class="grid grid-cols-2 gap-2">
                            <input type="text" class="input font-mono" placeholder="username" bind:value={up.auth.username} />
                            <input type="password" class="input font-mono" placeholder="password" bind:value={up.auth.password} />
                          </div>
                        {:else if up.auth?.type === 'bearer'}
                          <input type="password" class="input font-mono" placeholder="token" bind:value={up.auth.token} />
                        {:else if up.auth?.type === 'header'}
                          <div class="grid grid-cols-2 gap-2">
                            <input type="text" class="input font-mono" placeholder="X-Header-Name" bind:value={up.auth.header} />
                            <input type="password" class="input font-mono" placeholder="value" bind:value={up.auth.value} />
                          </div>
                        {/if}

                        <details>
                          <summary class="cursor-pointer text-[11px] text-slate-500 dark:text-warm-400 hover:text-warm-700 dark:hover:text-warm-300">
                            SSH private key (optional)
                          </summary>
                          <textarea
                            class="mt-1 w-full px-2 py-1 rounded border border-warm-300 dark:border-warm-700 bg-white dark:bg-warm-800 font-mono text-[12px]"
                            rows="3"
                            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
                            bind:value={up.ssh_key}
                          ></textarea>
                          <span class="block mt-1 text-[11px] text-slate-500 dark:text-warm-400 leading-relaxed">
                            Stored sealed for a future git-over-SSH fetch mode for private modules.
                            Not used by the current HTTP pull-through cache yet.
                          </span>
                        </details>
                      </div>
                    {/each}

                    {#if (repoDraft.upstreams ?? []).length === 0}
                      <div class="text-[11px] text-slate-500 dark:text-warm-400">
                        No prefix upstreams — every module uses the upstream URL above.
                      </div>
                    {/if}
                  </div>
                {/if}
              </div>
            {:else}
              <!-- ── Virtual: member list + default-local hint ── -->
              <div class="rounded border border-slate-200 dark:border-warm-700 p-3 bg-warm-50/50 dark:bg-warm-950/30 space-y-3">
                <label class="block">
                  <span class="field-label block mb-1.5">
                    Members <span class="text-vermilion-500">*</span>
                  </span>
                  <input
                    type="text"
                    class="input font-mono"
                    bind:value={virtualMembersText}
                    placeholder="local-repo, proxy-cache"
                  />
                  <span class="field-hint block mt-1">
                    Comma-separated sibling repository names (within {modeNamespace(mode)}). Lookup tries them in order; first match wins.
                  </span>
                  {#if siblingRepoNames.length > 0}
                    <span class="field-hint block mt-1">
                      Available: <code class="font-mono">{siblingRepoNames.join(', ')}</code>
                    </span>
                  {/if}
                </label>

                <label class="block">
                  <span class="field-label block mb-1.5">Default local (hint)</span>
                  <input
                    type="text"
                    class="input font-mono"
                    bind:value={repoDraft.default_local}
                    placeholder="Optional — which local member receives writes"
                  />
                </label>
              </div>
            {/if}

            <RepoPolicyEditor bind:draft={repoDraft} />

            <!-- ── B6: common per-repo overrides ────────────────────
                 cors_origins and max_upload_size apply to any kind
                 (local/remote/virtual). Live below the kind-specific
                 fields so the form reads top-to-bottom: identity →
                 kind selector → kind-specific fields → common
                 overrides. -->
            <details class="border-t border-slate-200 dark:border-warm-700 pt-3 mt-3">
              <summary class="cursor-pointer text-[13px] font-medium text-warm-600 dark:text-warm-400 hover:text-warm-900 dark:hover:text-warm-100">
                Advanced (CORS, upload size)
              </summary>
              <div class="space-y-3 mt-3">
                <label class="block">
                  <span class="field-label block mb-1.5">
                    CORS origins
                  </span>
                  <input
                    type="text"
                    class="input font-mono"
                    bind:value={corsOriginsText}
                    placeholder="https://app.example.com, *"
                  />
                  <span class="field-hint block mt-1">
                    Comma-separated origins allowed for browser-based clients.
                    Use <code class="font-mono">*</code> as a single entry for
                    permissive CORS. Empty disables CORS headers (server-default
                    behaviour).
                  </span>
                </label>

                <div>
                  <span class="field-label block mb-1.5">
                    Max upload size
                  </span>
                  <div class="flex items-center gap-2">
                    <input
                      type="number"
                      min="0"
                      step="1"
                      class="w-24 px-2 py-1 rounded border border-warm-300 dark:border-warm-700 bg-white dark:bg-warm-800 font-mono text-[13px]"
                      bind:value={maxUploadValue}
                      placeholder="0"
                    />
                    <select
                      class="px-2 py-1 rounded border border-warm-300 dark:border-warm-700 bg-white dark:bg-warm-800 text-[13px]"
                      bind:value={maxUploadUnit}
                    >
                      <option value="MB">MB</option>
                      <option value="GB">GB</option>
                    </select>
                    <span class="text-[11px] text-slate-500 dark:text-warm-400">0 = type default</span>
                  </div>
                  <span class="field-hint block mt-1">
                    Caps the publish / push body size. Server-side defaults: NPM
                    200 MB, Docker (per-blob) varies by upload session.
                  </span>
                </div>
              </div>
            </details>
        </div>

      <div class="flex items-center justify-end gap-2 mt-4">
        <button
          class="px-3 py-1 text-[13px] rounded border border-warm-300 dark:border-warm-700 hover:bg-warm-100 dark:hover:bg-warm-800"
          onclick={cancelModal}
          disabled={saving}
        >
          Cancel
        </button>
        <button
          class="px-3 py-1 text-[13px] rounded bg-accent-500 hover:bg-accent-600 text-white disabled:opacity-50 flex items-center gap-1"
          onclick={saveRepository}
          disabled={saving}
        >
          {#if saving}<Loader2 size={12} class="animate-spin" />{/if}
          Save
        </button>
      </div>
    </div>
  {/snippet}

  <!-- Side-panel package detail. Mounted at the page root so it
       overlays everything (namespace sidebar, repo list, detail
       column) instead of nesting inside the detail column where
       it would be clipped by the column's overflow rules. -->
  <RegistrySearch
    open={searchOpen}
    onclose={() => (searchOpen = false)}
    onpick={(hit) => {
      searchOpen = false;
      const ns = namespaces.find((n) => n.name === hit.namespace);
      const r = ns?.repositories?.find((x) => x.name === hit.repo);
      if (!ns || !r) return;
      selectedNS = ns.name;
      selectRepo(r);
      openDetail(hit.name, r.type);
    }}
  />

  {#if detailPackage && selectedRepo && selectedNS}
    <PackageDetailPanel
      namespace={selectedNS}
      repoName={selectedRepo.name}
      repoType={detailPackage.type}
      packageName={detailPackage.name}
      endpoint={endpointURL(selectedNS, selectedRepo.name)}
      canDelete={canDelete}
      ondeleted={refreshAfterArtifactDelete}
      onclose={closeDetail}
    />
  {/if}
</div>
