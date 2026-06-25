import { useEffect } from 'react';
import type { ToastType } from '../lib/toast';

interface ToastProps {
  message: string;
  type: ToastType;
  onDismiss: () => void;
}

export function Toast({ message, type, onDismiss }: ToastProps) {
  useEffect(() => {
    const timer = setTimeout(onDismiss, 5000);
    return () => clearTimeout(timer);
  }, [onDismiss]);

  const colors = {
    error: 'bg-red-500/90 border-red-400',
    success: 'bg-green-500/90 border-green-400',
    info: 'bg-[var(--color-accent)]/90 border-[var(--color-accent)]',
  };

  return (
    <div className={`fixed top-4 right-4 z-[100] ${colors[type]} border text-white px-4 py-3 rounded-lg shadow-lg text-sm flex items-center gap-2 max-w-md animate-in`}>
      <span className="flex-1">{message}</span>
      <button onClick={onDismiss} className="text-white/70 hover:text-white text-lg leading-none">&times;</button>
    </div>
  );
}
