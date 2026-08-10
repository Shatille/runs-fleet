import { Instance } from '@/lib/types';

// Instances a terminate would be a no-op on: already gone or on their way out.
const GONE_STATES = ['terminated', 'shutting-down'];

export function isTerminableState(state?: string): boolean {
  return !GONE_STATES.includes(state?.toLowerCase() || '');
}

export function isTerminable(instance: Instance): boolean {
  return isTerminableState(instance.state);
}
