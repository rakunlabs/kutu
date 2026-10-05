<script lang="ts">
 import AppCodeMirror from '@/lib/editor/AppCodeMirror.svelte';
 import { json } from '@codemirror/lang-json';
 import { yaml } from '@codemirror/lang-yaml';
 import { StreamLanguage, LanguageSupport } from '@codemirror/language';
 import { toml } from '@codemirror/legacy-modes/mode/toml';
 import { EditorState } from '@codemirror/state';
 import { filesStore } from '@/lib/store/files.svelte';
 import HexViewer from '@/lib/components/config/HexViewer.svelte';
 import { FileText, FileQuestion, Eye, Loader2, Download, AlertTriangle } from 'lucide-svelte';

 function getLanguageExtension(name: string): LanguageSupport | undefined {
 const lang = filesStore.getLanguageFromExt(name);
 switch (lang) {
 case 'json': return json();
 case 'yaml': return yaml();
 case 'toml': return new LanguageSupport(StreamLanguage.define(toml));
 default: return undefined;
 }
 }

 const activeTab = $derived(filesStore.activeTab);
 const languageExtension = $derived(activeTab ? getLanguageExtension(activeTab.name) : undefined);
 const downloadUrl = $derived(activeTab ? filesStore.getDownloadUrl(activeTab) : '');

 function handleForceOpen() {
 if (activeTab) {
 filesStore.forceOpenHex(activeTab.id);
 }
 }

 function handleDownload() {
 if (!activeTab) return;
 const a = document.createElement('a');
 a.href = downloadUrl;
 a.download = activeTab.name;
 document.body.appendChild(a);
 a.click();
 document.body.removeChild(a);
 }
</script>

<div class="flex flex-col h-full bg-white dark:bg-warm-950">
 {#if activeTab}
 <!-- Toolbar -->
 <div class="flex items-center justify-between px-3 py-1.5 bg-slate-50 dark:bg-warm-900 border-b border-slate-200 dark:border-warm-700 text-[13px] text-slate-500 dark:text-warm-400 shrink-0">
 <div class="flex items-center gap-2 min-w-0">
 <span class="tag shrink-0">raw</span>
 <span class="font-mono text-slate-700 dark:text-warm-200 overflow-hidden text-ellipsis whitespace-nowrap" title="{activeTab.mount}/{activeTab.path}">
 {activeTab.mount}/{activeTab.path}
 </span>
 </div>
 <div class="flex items-center gap-1.5 shrink-0">
 {#if activeTab.contentType}
 <span class="tag normal-case tracking-normal" style="font-stretch:100%">
 {activeTab.contentType.split(';')[0]}
 </span>
 {/if}
 <span class="font-mono text-[12px] text-slate-500 dark:text-warm-400">{filesStore.formatSize(activeTab.size)}</span>
 <span class="status status-off px-1">Read-only</span>
 <button
 class="btn btn-secondary btn-sm"
 onclick={handleDownload}
 title="Download file"
 >
 <Download size={12} />
 Download
 </button>
 </div>
 </div>

 <!-- Content area -->
 <div class="flex-1 min-h-0 overflow-auto flex flex-col">
 {#if !activeTab.loaded}
 <!-- Loading state -->
 <div class="flex flex-col items-center justify-center flex-1 gap-3 text-slate-500 dark:text-warm-400">
 <Loader2 size={24} class="animate-spin" />
 <span class="text-[14px]">Loading file...</span>
 </div>

 {:else if activeTab.viewerMode === 'text'}
 <!-- Truncation warning -->
 {#if activeTab.truncated}
 <div class="flex items-center gap-2 px-3 py-1.5 bg-amber-900/30 border-b border-amber-700/50 text-amber-400 text-[13px] shrink-0">
 <AlertTriangle size={13} class="shrink-0" />
 <span>File is too large to display entirely. Showing first {filesStore.formatSize(filesStore.MAX_TEXT_SIZE)} of {filesStore.formatSize(activeTab.size)}.</span>
 <button
 class="ml-auto flex items-center gap-1 px-2 py-0.5 text-amber-300 bg-transparent border border-amber-700 rounded cursor-pointer text-[12px] transition-colors hover:bg-amber-800/50 hover:text-amber-200"
 onclick={handleDownload}
 >
 <Download size={11} />
 Download full file
 </button>
 </div>
 {/if}
 <!-- Text/Code viewer (read-only) -->
 <div class="flex-1 min-h-0">
 <AppCodeMirror
 value={activeTab.textContent || ''}
 lang={languageExtension}
 readonly={true}
 extensions={[EditorState.readOnly.of(true)]}
 />
 </div>

 {:else if activeTab.viewerMode === 'image'}
 <!-- Image viewer -->
 <div class="flex items-center justify-center flex-1 p-8 bg-slate-100 dark:bg-warm-950">
 <div class="max-w-full max-h-full overflow-auto rounded-[3px] shadow-[0_8px_24px_-6px_rgb(0_0_0/0.3)] leaf p-4">
 <img
 src={activeTab.rawUrl}
 alt={activeTab.name}
 class="max-w-full max-h-[calc(100vh-200px)] object-contain"
 />
 </div>
 </div>

 {:else if activeTab.viewerMode === 'video'}
 <!-- Video player -->
 <div class="flex items-center justify-center flex-1 p-8 bg-slate-100 dark:bg-warm-950">
 <div class="max-w-full max-h-full rounded-[3px] shadow-[0_8px_24px_-6px_rgb(0_0_0/0.3)] leaf p-4">
 <!-- svelte-ignore a11y_media_has_caption -->
 <video
 src={activeTab.rawUrl}
 controls
 class="max-w-full max-h-[calc(100vh-200px)] rounded"
 >
 Your browser does not support the video element.
 </video>
 </div>
 </div>

 {:else if activeTab.viewerMode === 'audio'}
 <!-- Audio player -->
 <div class="flex flex-col items-center justify-center flex-1 gap-6 bg-slate-100 dark:bg-warm-950">
 <div class="flex items-center justify-center w-20 h-20 rounded-[3px] leaf">
 <svg xmlns="http://www.w3.org/2000/svg" width="36" height="36" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" class="text-accent-500"><path d="M9 18V5l12-2v13"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/></svg>
 </div>
 <div class="text-center">
 <p class="text-[14px] font-semibold text-slate-800 dark:text-warm-100 mb-1">{activeTab.name}</p>
 <p class="text-[12px] text-slate-500 dark:text-warm-400">{filesStore.formatSize(activeTab.size)}</p>
 </div>
 <audio
 src={activeTab.rawUrl}
 controls
 class="w-full max-w-md"
 >
 Your browser does not support the audio element.
 </audio>
 </div>

 {:else if activeTab.viewerMode === 'pdf'}
 <!-- PDF viewer -->
 <iframe
 src={activeTab.rawUrl}
 title={activeTab.name}
 class="w-full flex-1 border-none"
 ></iframe>

 {:else if activeTab.viewerMode === 'hex'}
 <!-- Hex viewer (after user clicked "Open Anyway") -->
 <HexViewer data={activeTab.hexData || ''} />

 {:else if activeTab.viewerMode === 'binary-placeholder'}
 <!-- Binary file placeholder -->
 <div class="flex flex-col items-center justify-center flex-1 gap-4 text-slate-500 dark:text-warm-400">
 <div class="flex items-center justify-center w-16 h-16 rounded-[3px] leaf">
 <FileQuestion size={32} class="text-slate-500 dark:text-warm-400" />
 </div>
 <div class="text-center">
 <h3 class="text-[15px] font-semibold text-slate-800 dark:text-warm-100 mb-1">Binary file</h3>
 <p class="text-[13px] text-slate-500 dark:text-warm-400 mb-1">{activeTab.name}</p>
 <p class="text-[12px] text-slate-600 dark:text-warm-300">{filesStore.formatSize(activeTab.size)}</p>
 {#if activeTab.contentType}
 <p class="text-[12px] text-slate-600 dark:text-warm-300 mt-0.5">{activeTab.contentType.split(';')[0]}</p>
 {/if}
 </div>

 {#if activeTab.tooLargeForHex}
 <!-- File too large for hex viewer -->
 <div class="flex items-center gap-2 px-4 py-2.5 bg-amber-50 dark:bg-amber-950/40 rounded-[3px] text-amber-900 dark:text-amber-100 text-[13px] max-w-sm">
 <AlertTriangle size={14} class="shrink-0" />
 <span>File is too large for the hex viewer (limit: {filesStore.formatSize(filesStore.MAX_HEX_SIZE)}). Use the download button instead.</span>
 </div>
 <button
 class="btn btn-primary"
 onclick={handleDownload}
 >
 <Download size={16} />
 Download File
 </button>
 {:else}
 <button
 class="btn btn-secondary"
 onclick={handleForceOpen}
 >
 <Eye size={16} />
 Open Anyway
 </button>
 <p class="text-[12px] text-slate-600 dark:text-warm-300 max-w-xs text-center">
 The file will be downloaded and displayed as a hex dump viewer.
 </p>
 {/if}
 </div>

 {:else if activeTab.viewerMode === 'binary-loading'}
 <!-- Loading hex data after user clicked Open Anyway -->
 <div class="flex flex-col items-center justify-center flex-1 gap-3 text-slate-500 dark:text-warm-400">
 <Loader2 size={24} class="animate-spin" />
 <span class="text-[14px]">Loading binary data...</span>
 </div>
 {/if}
 </div>
 {:else}
 <!-- No file open -->
 <div class="flex items-center justify-center h-full bg-slate-50 dark:bg-warm-900">
 <div class="flex flex-col items-center gap-2 text-center px-6">
 <FileText size={28} class="text-accent-500" />
 <h3 class="text-[14px] font-semibold text-slate-800 dark:text-warm-100">No file open</h3>
 <p class="text-[13px] text-slate-500 dark:text-warm-400 max-w-xs">Pick a file in the tree to view it here. Double-click a folder to expand it.</p>
 </div>
 </div>
 {/if}
</div>
