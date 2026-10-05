import NotFound from '@/pages/NotFound.svelte';
import Registries from '@/pages/Registries.svelte';
import Files from '@/pages/Files.svelte';
import Settings from '@/pages/Settings.svelte';
import Listeners from '@/pages/Listeners.svelte';

export default {
  '/': Registries,
  '/registries': Registries,
  '/files': Files,
  '/listeners': Listeners,
  '/listeners/:section': Listeners,
  '/settings': Settings,
  '/settings/:section': Settings,
  '*': NotFound,
};
