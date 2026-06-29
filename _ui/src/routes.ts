import NotFound from '@/pages/NotFound.svelte';
import Registries from '@/pages/Registries.svelte';
import Files from '@/pages/Files.svelte';
import Proxy from '@/pages/Proxy.svelte';
import Settings from '@/pages/Settings.svelte';

export default {
  '/': Registries,
  '/registries': Registries,
  '/files': Files,
  '/proxy': Proxy,
  '/settings': Settings,
  '*': NotFound,
};
