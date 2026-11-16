import { ToastType } from '@ctrliq/quantic-components';

import { addToast, dismissToast } from './store';

type ToastOptions = {
  type?: ToastType;
  description?: string;
  duration?: number;
};

export const toastDurationMs = 2800;

const withType =
  (type: ToastType) =>
  (title: string, options: ToastOptions = {}): string =>
    addToast({
      title,
      type: options.type ?? type,
      description: options.description,
      duration: options.duration ?? toastDurationMs,
    });

export const toast = Object.assign(withType(ToastType.Info), {
  success: withType(ToastType.Success),
  error: withType(ToastType.Error),
  warning: withType(ToastType.Warning),
  dismiss: dismissToast,
});
