import NotFound from '@/pages/NotFound.svelte';
import Registries from '@/pages/Registries.svelte';
import Files from '@/pages/Files.svelte';
import Settings from '@/pages/Settings.svelte';

export default {
  '/': Registries,
  '/registries': Registries,
  '/files': Files,
  '/settings': Settings,
  '*': NotFound,
};
