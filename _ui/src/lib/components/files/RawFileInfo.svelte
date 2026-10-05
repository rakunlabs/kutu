<script lang="ts">
 import { filesStore } from '@/lib/store/files.svelte';
 import { FileText, HardDrive, Folder, File, Image as ImageIcon, FileCode, Binary, Video, Music } from 'lucide-svelte';

 const tab = $derived(filesStore.activeTab);

 function getViewerLabel(mode: string): string {
 switch (mode) {
 case 'text': return 'Text / Code';
 case 'image': return 'Image';
 case 'video': return 'Video';
 case 'audio': return 'Audio';
 case 'pdf': return 'PDF Document';
 case 'hex': return 'Hex Dump';
 case 'binary-placeholder': return 'Binary';
 default: return 'Unknown';
 }
 }

 function getFileExtension(name: string): string {
 const lastDot = name.lastIndexOf('.');
 if (lastDot <= 0) return '-';
 return name.slice(lastDot);
 }
</script>

<div class="flex flex-col h-full bg-slate-50 dark:bg-warm-950/60 border-l border-slate-200 dark:border-warm-700 select-none">
 <!-- Header -->
 <div class="flex items-center px-3 py-2 border-b border-slate-200 dark:border-warm-700">
 <span class="label-caps text-[11px] text-slate-500 dark:text-warm-400">File Info</span>
 </div>

 {#if tab}
 <div class="flex-1 overflow-y-auto p-3">
 <!-- File name -->
 <div class="mb-4">
 <div class="flex items-center gap-2 mb-1">
 <FileText size={16} class="text-slate-500 dark:text-warm-400 shrink-0" />
 <span class="text-[14px] font-medium text-slate-800 dark:text-warm-100 break-all">{tab.name}</span>
 </div>
 </div>

 <!-- Properties -->
 <div class="space-y-3">
 <!-- Mount -->
 <div>
 <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Mount</div>
 <div class="flex items-center gap-1.5 text-[13px] text-slate-700 dark:text-warm-200">
 <HardDrive size={12} class="text-accent-600 dark:text-accent-300 shrink-0" />
 <span>{tab.mount}</span>
 </div>
 </div>

 <!-- Path -->
 <div>
 <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Path</div>
 <div class="flex items-start gap-1.5 text-[13px] text-slate-700 dark:text-warm-200">
 <Folder size={12} class="text-amber-500 shrink-0 mt-0.5" />
 <span class="break-all font-mono text-[12px]">{tab.path}</span>
 </div>
 </div>

 <!-- Size -->
 <div>
 <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Size</div>
 <div class="flex items-center gap-1.5 text-[13px] text-slate-700 dark:text-warm-200">
 <File size={12} class="text-slate-500 dark:text-warm-400 shrink-0" />
 <span>{filesStore.formatSize(tab.size)}</span>
 <span class="text-slate-500 dark:text-warm-400">({tab.size.toLocaleString()} bytes)</span>
 </div>
 </div>

 <!-- Extension -->
 <div>
 <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Extension</div>
 <div class="text-[13px] text-slate-700 dark:text-warm-200">
 <span class="px-1.5 py-0.5 bg-slate-200 dark:bg-warm-900 rounded font-mono text-[12px]">{getFileExtension(tab.name)}</span>
 </div>
 </div>

 <!-- Content Type -->
 {#if tab.contentType}
 <div>
 <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Content Type</div>
 <div class="text-[13px] text-slate-700 dark:text-warm-200 font-mono text-[12px] break-all">
 {tab.contentType.split(';')[0]}
 </div>
 </div>
 {/if}

 <!-- Viewer Mode -->
 <div>
 <div class="label-caps text-[11px] text-slate-500 dark:text-warm-400 mb-1">Viewer</div>
 <div class="flex items-center gap-1.5 text-[13px] text-slate-700 dark:text-warm-200">
 {#if tab.viewerMode === 'text'}
 <FileCode size={12} class="text-accent-600 dark:text-accent-300 shrink-0" />
 {:else if tab.viewerMode === 'image'}
 <ImageIcon size={12} class="text-green-500 shrink-0" />
 {:else if tab.viewerMode === 'video'}
 <Video size={12} class="text-pink-500 shrink-0" />
 {:else if tab.viewerMode === 'audio'}
 <Music size={12} class="text-purple-500 shrink-0" />
 {:else if tab.viewerMode === 'hex'}
 <Binary size={12} class="text-amber-500 shrink-0" />
 {:else}
 <FileText size={12} class="text-slate-500 dark:text-warm-400 shrink-0" />
 {/if}
 <span>{getViewerLabel(tab.viewerMode)}</span>
 </div>
 </div>
 </div>
 </div>
 {:else}
 <div class="flex-1 flex items-center justify-center p-4">
 <span class="text-[13px] text-slate-500 dark:text-warm-400">Select a file to see details</span>
 </div>
 {/if}
</div>
