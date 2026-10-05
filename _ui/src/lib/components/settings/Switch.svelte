<script lang="ts">
 let {
  checked,
  disabled = false,
  label,
  onchange,
 }: {
  checked: boolean;
  disabled?: boolean;
  label: string;
  onchange: () => void;
 } = $props();
</script>

<button
 type="button"
 role="switch"
 aria-checked={checked}
 aria-label={label}
 title={label}
 {disabled}
 onclick={onchange}
 class="sw shrink-0 {checked ? 'is-on' : ''}"
><span></span></button>

<style>
 .sw {
  position: relative;
  width: 2.25rem;
  height: 1.25rem;
  border-radius: 999px;
  background: var(--color-slate-300);
  cursor: pointer;
  transition: background-color 90ms steps(2);
 }

 :global(.dark) .sw {
  background: var(--color-warm-600);
 }

 .sw span {
  position: absolute;
  top: 2px;
  left: 2px;
  width: 1rem;
  height: 1rem;
  border-radius: 999px;
  background: #fff;
  box-shadow: 0 1px 2px rgb(0 0 0 / 0.25);
  transition: transform 90ms steps(2);
 }

 .sw.is-on {
  background: var(--sec-600);
 }

 :global(.dark) .sw.is-on {
  background: var(--sec-500);
 }

 .sw.is-on span {
  transform: translateX(1rem);
 }

 .sw:disabled {
  opacity: 0.5;
  cursor: progress;
 }
</style>
