import { Toast } from '@ctrliq/quantic-react';
import { createPortal } from 'react-dom';

import { dismissToast, useToastStore } from './store';
import styles from './Toaster.module.scss';

export const Toaster = () =>
  createPortal(
    <div className={styles.toaster} role="status" aria-live="polite" aria-atomic="false">
      {useToastStore().map((entry) => (
        <Toast
          key={entry.id}
          type={entry.type}
          open={entry.open}
          onClose={() => {
            dismissToast(entry.id);
          }}
        >
          <span slot="title">{entry.title}</span>
          {entry.description}
        </Toast>
      ))}
    </div>,
    document.body,
  );
