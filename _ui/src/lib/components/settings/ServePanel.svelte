<script lang="ts">
 // File-serving panel — operator-facing config for kutu's built-in file
 // servers. Three independently-saved sections:
 //
 //   Servers : any number of FTP / SFTP / TFTP / WebDAV / S3 instances,
 //             each on its own port and each exposing all shares or a
 //             picked subset.
 //   Shares  : the global pool mapping names to raw-mount paths.
 //   Users   : the global credential pool for FTP / SFTP / WebDAV / S3
 //             (TFTP is anonymous + read-only).
 //
 // Every section persists its own edits with explicit Save / Cancel
 // (via serveStore partial saves), so there is no page-wide dirty
 // draft. A save reconciles the running servers server-side; the status
 // badges refresh right after and can be refreshed manually here.
 import { Server, RefreshCw } from 'lucide-svelte';
 import { serveStore } from '@/lib/store/serve.svelte';
 import ServeServersSection from './ServeServersSection.svelte';
 import ServeSharesSection from './ServeSharesSection.svelte';
 import ServeUsersSection from './ServeUsersSection.svelte';

 let refreshing = $state(false);
 async function refresh() {
  refreshing = true;
  try { await serveStore.refreshStatus(); } finally { refreshing = false; }
 }
</script>

<div>
 <header class="mb-5 flex items-start justify-between gap-4">
  <div>
   <h2 class="text-base font-semibold text-slate-800 dark:text-slate-100 flex items-center gap-2">
    <Server size={18} class="text-accent-600 dark:text-accent-400" />
    File serving
   </h2>
   <p class="text-sm text-slate-500 dark:text-slate-400 mt-1">
    Expose raw mounts over <strong>FTP</strong>, <strong>SFTP</strong>, <strong>TFTP</strong>, <strong>WebDAV</strong> and the <strong>S3 API</strong> —
    run as many server instances as you need, each on its own port and with its own share selection.
    Users provide the credentials (TFTP is anonymous and read-only); over S3, shares appear as buckets
    and a user's username/password act as the access/secret key pair.
   </p>
  </div>
  <button
   type="button"
   class="px-3 py-1.5 text-xs rounded bg-slate-100 dark:bg-warm-900 text-slate-700 dark:text-slate-300 hover:bg-slate-200 dark:hover:bg-warm-700 inline-flex items-center gap-1.5 cursor-pointer shrink-0"
   title="Refresh server status"
   onclick={refresh}
   disabled={refreshing}
  >
   <RefreshCw size={12} class={refreshing ? 'animate-spin' : ''} /> Status
  </button>
 </header>

 <ServeServersSection />
 <ServeSharesSection />
 <ServeUsersSection />
</div>
