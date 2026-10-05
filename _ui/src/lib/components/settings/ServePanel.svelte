<script lang="ts">
 // File serving — kutu's built-in file servers. Three independently
 // saved sections:
 //
 //   Servers : any number of FTP / SFTP / TFTP / WebDAV / S3 instances,
 //             each on its own port and exposing all shares or a subset.
 //   Shares  : the global pool mapping names to raw-mount paths.
 //   Users   : the global credential pool (TFTP is anonymous + read-only).
 //
 // Every section persists its own edits with explicit Save / Cancel
 // (via serveStore partial saves), so there is no page-wide dirty draft.
 import { RefreshCw } from 'lucide-svelte';
 import { serveStore } from '@/lib/store/serve.svelte';
 import PanelHeader from './PanelHeader.svelte';
 import ServeServersSection from './ServeServersSection.svelte';
 import ServeSharesSection from './ServeSharesSection.svelte';
 import ServeUsersSection from './ServeUsersSection.svelte';

 let refreshing = $state(false);
 async function refresh() {
  refreshing = true;
  try { await serveStore.refreshStatus(); } finally { refreshing = false; }
 }
</script>

<PanelHeader title="File serving">
 Expose raw mounts over FTP, SFTP, TFTP, WebDAV and an S3-compatible API. Servers pick which shares they expose; users hold the credentials.
 {#snippet actions()}
  <button type="button" class="btn btn-secondary" onclick={refresh} disabled={refreshing} title="Re-read the running state of every server">
   <RefreshCw size={14} class={refreshing ? 'animate-spin' : ''} /> Refresh status
  </button>
 {/snippet}
</PanelHeader>

<div class="flex flex-col gap-10">
 <ServeServersSection />
 <ServeSharesSection />
 <ServeUsersSection />
</div>
