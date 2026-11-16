import { useScreenPlace } from './useScreenPlace';

export const useNamespaceFilter = (): string | undefined => useScreenPlace().namespace;
