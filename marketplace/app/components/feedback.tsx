import { useEffect, useState } from "react";
import BasicToast, { type ToastType } from "../../components/ui/smoothui/basic-toast";

/**
 * Action feedback via the smoothui `basic-toast`. Re-arms whenever the
 * message changes so consecutive fetcher results each get a fresh toast.
 */
export function ActionToast({
  message,
  type = "error",
  duration = 5000,
}: {
  message: string | null | undefined;
  type?: ToastType;
  duration?: number;
}) {
  const [visible, setVisible] = useState(false);

  useEffect(() => {
    setVisible(Boolean(message));
  }, [message]);

  if (!message) return null;
  return (
    <BasicToast
      key={message}
      message={message}
      type={type}
      duration={duration}
      isVisible={visible}
      onClose={() => setVisible(false)}
    />
  );
}
