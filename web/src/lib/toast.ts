import { useEffect, useState } from 'react';

export type ToastType = 'error' | 'success' | 'info';

export type ToastState = {
  message: string;
  type: ToastType;
};

type ToastListener = (toast: ToastState) => void;

const listeners = new Set<ToastListener>();

export function showToast(message: string, type: ToastType = 'info') {
  for (const listener of listeners) {
    listener({ message, type });
  }
}

export function useToast() {
  const [toast, setToast] = useState<ToastState | null>(null);

  useEffect(() => {
    listeners.add(setToast);
    return () => {
      listeners.delete(setToast);
    };
  }, []);

  return {
    toast,
    dismissToast: () => setToast(null),
  };
}
