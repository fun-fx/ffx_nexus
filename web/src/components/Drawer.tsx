import { useEffect, useRef } from "react";
import { Icon } from "./icons";

interface Props {
  open: boolean;
  title: React.ReactNode;
  onClose: () => void;
  footer?: React.ReactNode;
  children: React.ReactNode;
}

/**
 * Right-edge sliding drawer. Uses a focus-trap shell that focuses the first
 * text input in the body on mount (so close buttons in the header do not
 * steal the caret on open) and returns focus to the previously-focused
 * element on close.
 */
export function Drawer({ open, title, onClose, footer, children }: Props) {
  const ref = useRef<HTMLDivElement | null>(null);
  const lastFocused = useRef<HTMLElement | null>(null);

  useEffect(() => {
    if (!open) return;
    lastFocused.current = document.activeElement as HTMLElement | null;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        onClose();
      }
    };
    document.addEventListener("keydown", onKey);
    document.body.style.overflow = "hidden";
    queueMicrotask(() => {
      const node = ref.current;
      if (!node) return;
      // Prefer the first text input/textarea in the body so typing can start
      // immediately. Fall back to the first focusable child only when the
      // drawer has nothing to type into (e.g. confirm dialogs). The header
      // close button shows the focus halo if it wins the querySelector race,
      // which made dialogs feel like the caret escaped on every keypress.
      const textField = node.querySelector<HTMLInputElement | HTMLTextAreaElement>(
        ".drawer-body input:not([type=hidden]):not([type=button]):not([type=submit]), .drawer-body textarea, .drawer-body [autofocus]",
      );
      if (textField) {
        textField.focus();
        return;
      }
      // No text field in the body (e.g. confirm/result dialogs) — focus the
      // first interactive element inside the body. Scoping to .drawer-body
      // is deliberate: the header close button should never win this race
      // because doing so makes the caret appear to "escape" the input.
      const fallback = node.querySelector<HTMLElement>(
        '.drawer-foot button:enabled, .drawer-body button:enabled, .drawer-foot [tabindex]:not([tabindex="-1"]):not([disabled]), .drawer-body [tabindex]:not([tabindex="-1"]):not([disabled])',
      );
      fallback?.focus();
    });
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = "";
      lastFocused.current?.focus?.();
    };
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div className="drawer-overlay" role="presentation" onClick={onClose}>
      <div
        ref={ref}
        className="drawer"
        role="dialog"
        aria-modal="true"
        aria-label={typeof title === "string" ? title : undefined}
        onClick={(e) => e.stopPropagation()}
      >
        <header className="drawer-head">
          <h2 className="drawer-title">{title}</h2>
          <button
            className="btn-ghost drawer-close"
            type="button"
            onClick={onClose}
            aria-label="Close"
          >
            <Icon.x size={16} />
          </button>
        </header>
        <div className="drawer-body">{children}</div>
        {footer && <footer className="drawer-foot">{footer}</footer>}
      </div>
    </div>
  );
}
