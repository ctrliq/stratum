import { ToastType } from '@ctrliq/quantic-components';
import { useSyncExternalStore } from 'react';

export type { ToastType } from '@ctrliq/quantic-components';

export type ToastEntry = {
  id: string;
  type: ToastType;
  title: string;
  description?: string;
  duration: number;
  open: boolean;
};

type ToastInput = Omit<ToastEntry, 'id' | 'open'>;

const exitDurationMs = 300;

const store: { toasts: readonly ToastEntry[]; issued: number } = { toasts: [], issued: 0 };
const subscribers = new Set<() => void>();

const withToasts = (toasts: readonly ToastEntry[]): void => {
  store.toasts = toasts;
  for (const onChanged of subscribers) {
    onChanged();
  }
};

export const dismissToast = (id: string): void => {
  if (store.toasts.some((entry) => entry.id === id && entry.open)) {
    withToasts(store.toasts.map((entry) => (entry.id === id ? { ...entry, open: false } : entry)));
    setTimeout(() => {
      withToasts(store.toasts.filter((entry) => entry.id !== id));
    }, exitDurationMs);
  }
};

export const addToast = (input: ToastInput): string => {
  store.issued += 1;
  const id = String(store.issued);
  withToasts([{ ...input, id, open: true }]);
  if (input.duration > 0) {
    setTimeout(() => {
      dismissToast(id);
    }, input.duration);
  }
  return id;
};

export const useToastStore = (): readonly ToastEntry[] =>
  useSyncExternalStore(
    (onChanged) => {
      subscribers.add(onChanged);
      return () => subscribers.delete(onChanged);
    },
    () => store.toasts,
  );
