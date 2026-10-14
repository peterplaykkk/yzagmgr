import {
  useId,
  useRef,
  useState,
  type FocusEvent,
  type KeyboardEvent,
  type ReactNode,
} from 'react';
import Box from '@mui/material/Box';
import ClickAwayListener from '@mui/material/ClickAwayListener';
import IconButton from '@mui/material/IconButton';
import Paper from '@mui/material/Paper';
import Popper from '@mui/material/Popper';
import Typography from '@mui/material/Typography';
import HelpOutlineIcon from '@mui/icons-material/HelpOutline';
import { fontWeight, layout, radius, shadow, spacing } from '../theme';

export interface ContextHelpProps {
  /** Localized, topic-specific accessible name for the help control. */
  label: string;
  /** Short, task-specific guidance. Avoid repeating explanations already visible on the page. */
  children: ReactNode;
  /** Optional localized heading for multi-sentence help. */
  heading?: string;
  /** Optional stable id for page-level references. */
  id?: string;
  /** The panel flips to the opposite side when this side lacks room. */
  placement?: 'right-start' | 'left-start';
}

/**
 * Compact contextual help that works on pointer, keyboard, and touch. It opens on click or
 * keyboard focus; hover is not required. Escape closes and restores focus to the trigger.
 */
export function ContextHelp({
  label,
  children,
  heading,
  id,
  placement = 'right-start',
}: ContextHelpProps) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const panelRef = useRef<HTMLDivElement | null>(null);
  const pointerDown = useRef(false);
  const openedByFocus = useRef(false);
  const suppressFocusOpen = useRef(false);
  const generatedId = useId().replace(/[^a-zA-Z0-9_-]/g, '');
  const panelId = id ?? `context-help-${generatedId}`;
  const headingId = `${panelId}-heading`;

  const close = (restoreFocus = false) => {
    setOpen(false);
    openedByFocus.current = false;
    if (restoreFocus) {
      suppressFocusOpen.current = true;
      triggerRef.current?.focus();
      queueMicrotask(() => {
        suppressFocusOpen.current = false;
      });
    }
  };

  const handleTriggerFocus = () => {
    const fromPointer = pointerDown.current;
    pointerDown.current = false;
    if (suppressFocusOpen.current || fromPointer) return;
    openedByFocus.current = true;
    setOpen(true);
  };

  const handleBlur = (event: FocusEvent<HTMLElement>) => {
    const next = event.relatedTarget;
    if (
      next instanceof Node &&
      (next === triggerRef.current || panelRef.current?.contains(next) === true)
    ) {
      return;
    }
    close();
  };

  const handleKeyDown = (event: KeyboardEvent<HTMLElement>) => {
    if (event.key !== 'Escape' || !open) return;
    event.preventDefault();
    event.stopPropagation();
    close(true);
  };

  return (
    <ClickAwayListener onClickAway={() => close()}>
      <Box component="span" sx={{ display: 'inline-flex', alignItems: 'center' }}>
        <IconButton
          ref={triggerRef}
          type="button"
          size="small"
          aria-label={label}
          aria-expanded={open}
          aria-controls={open ? panelId : undefined}
          aria-describedby={open ? panelId : undefined}
          onPointerDown={() => {
            pointerDown.current = true;
          }}
          onFocus={handleTriggerFocus}
          onBlur={handleBlur}
          onClick={() => {
            if (openedByFocus.current) {
              openedByFocus.current = false;
              setOpen(true);
              return;
            }
            setOpen((value) => !value);
          }}
          onKeyDown={handleKeyDown}
          sx={{
            width: layout.contextHelpControlSize,
            height: layout.contextHelpControlSize,
          }}
        >
          <HelpOutlineIcon fontSize="small" />
        </IconButton>
        <Popper
          open={open}
          anchorEl={triggerRef.current}
          placement={placement}
          modifiers={[
            {
              name: 'flip',
              options: {
                fallbackPlacements:
                  placement === 'right-start'
                    ? ['left-start', 'bottom-start', 'top-start']
                    : ['right-start', 'bottom-start', 'top-start'],
                padding: 8,
              },
            },
            {
              name: 'preventOverflow',
              options: {
                boundary: 'viewport',
                rootBoundary: 'viewport',
                padding: 8,
                altAxis: true,
                tether: false,
              },
            },
          ]}
          sx={{ zIndex: (theme) => theme.zIndex.tooltip }}
          onKeyDown={handleKeyDown}
        >
          <Paper
            ref={panelRef}
            id={panelId}
            role="region"
            aria-label={heading === undefined ? label : undefined}
            aria-labelledby={heading === undefined ? undefined : headingId}
            onBlur={handleBlur}
            sx={{
              maxWidth: `min(${layout.contextHelpMaxWidth}px, calc(100vw - ${spacing.x4}))`,
              p: spacing.x3,
              border: 1,
              borderColor: 'divider',
              borderRadius: `${radius.md}px`,
              boxShadow: shadow.sm,
              backgroundImage: 'none',
            }}
          >
            {heading === undefined ? null : (
              <Typography
                id={headingId}
                component="h2"
                variant="body2"
                sx={{ mb: spacing.x1, fontWeight: fontWeight.semibold }}
              >
                {heading}
              </Typography>
            )}
            <Box
              sx={{
                color: 'text.secondary',
                minWidth: 0,
                overflowWrap: 'anywhere',
                '& p:first-of-type': { mt: 0 },
              }}
            >
              {children}
            </Box>
          </Paper>
        </Popper>
      </Box>
    </ClickAwayListener>
  );
}
