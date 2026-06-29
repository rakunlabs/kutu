<script lang="ts">
  import { link } from "svelte-spa-router";
</script>

<div class="flex h-full w-full items-center justify-center">
  <div class="text-center">
    <div class="text-5xl font-bold text-slate-300 dark:text-slate-600">404</div>
    <p class="mt-2 text-sm text-slate-500 dark:text-slate-400">Page not found</p>
    <a
      href="/"
      use:link
      class="mt-4 inline-block rounded bg-indigo-600 px-4 py-2 text-sm text-white hover:bg-indigo-500"
      >Go home</a
    >
  </div>
</div>
