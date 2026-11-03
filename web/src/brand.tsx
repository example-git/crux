import type { Provider, Session, Workspace } from './types.ts';

export type Brand = { title: string; gradientA: string; gradientB: string; accent: string };

export const defaultBrand: Brand = { title: 'CRUX', gradientA: '#39FF14', gradientB: '#FF3B1F', accent: '#39FF14' };

export function activeBrand(state: { currentSession?: Session; workspace?: Workspace; agent?: { model_cfg?: { provider?: string } } }) {
  const provider = state.currentSession?.models?.large?.model.provider || state.workspace?.config.models?.large?.provider || state.agent?.model_cfg?.provider;
  return brandFor(state.workspace?.provider_surfaces?.find(item => item.id === provider));
}

const background = '#201F26';
const hex = /^#[0-9a-f]{6}$/i;

export function brandFor(surface?: Provider): Brand {
  const declared = surface?.brand;
  if (!declared) return defaultBrand;
  const valid = (value?: string) => value && hex.test(value) ? value : '';
  const gradientA = valid(declared.gradient_a) || valid(declared.color) || defaultBrand.gradientA;
  const gradientB = valid(declared.gradient_b) || valid(declared.color) || defaultBrand.gradientB;
  return {
    title: declared.short_name || declared.label || defaultBrand.title,
    gradientA,
    gradientB,
    accent: valid(declared.color) || gradientA,
  };
}

function channels(value: string) {
  return [1, 3, 5].map(index => parseInt(value.slice(index, index + 2), 16));
}

function luminance(value: string) {
  const [r, g, b] = channels(value).map(channel => {
    const linear = channel / 255;
    return linear <= 0.04045 ? linear / 12.92 : ((linear + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(foreground: string, base: string) {
  const first = luminance(foreground), second = luminance(base);
  return (Math.max(first, second) + 0.05) / (Math.min(first, second) + 0.05);
}

function toHex(values: number[]) {
  return '#' + values.map(value => Math.round(value).toString(16).padStart(2, '0')).join('');
}

export function readableText(foreground: string, base = background) {
  if (contrast(foreground, base) >= 4.5) return foreground;
  const original = channels(foreground);
  for (let step = 1; step <= 255; step++) {
    const candidate = toHex(original.map(channel => channel + (255 - channel) * step / 255));
    if (contrast(candidate, base) >= 4.5) return candidate;
  }
  return foreground;
}

export function onAccent(value: string) {
  const [r, g, b] = channels(value);
  return (299 * r + 587 * g + 114 * b) / 1000 > 140 ? '#201F26' : '#FFFAF1';
}

function mix(value: string, base: string, amount: number) {
  const a = channels(value), b = channels(base);
  return toHex(a.map((channel, index) => b[index] + (channel - b[index]) * amount));
}

export function applyBrand(brand: Brand, root: HTMLElement = document.documentElement) {
  const text = readableText(brand.gradientA);
  const values: Record<string, string> = {
    '--brand-a': brand.gradientA,
    '--brand-b': brand.gradientB,
    '--brand-fill': brand.accent,
    '--accent': text,
    '--accent-strong': brand.accent,
    '--accent-bg': mix(brand.accent, background, 0.22),
    '--on-accent': onAccent(brand.accent),
  };
  for (const [name, value] of Object.entries(values)) root.style.setProperty(name, value);
  root.dataset.brand = brand.title;
}

export function Wordmark(props: { title: string; compact?: boolean }) {
  return <span class="wordmark" classList={{ compact: props.compact }} aria-label={props.title}>
    <span class="wordmark-field" aria-hidden="true" />
    <strong>{props.title}</strong>
    <span class="wordmark-field" aria-hidden="true" />
  </span>;
}
