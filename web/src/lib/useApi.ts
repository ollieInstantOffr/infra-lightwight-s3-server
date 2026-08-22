import { useCallback, useEffect, useState } from "react";
import { ApiError, api } from "./api";

/**
 * Loads data from the API, with the states a screen actually needs.
 *
 * A 401 is not reported as an error: the session simply ended, and the app
 * shell redirects to sign-in. Showing "Request failed (401)" on top of that
 * would be noise on the way to a login screen.
 */
export function useApi<T>(path: string | null): {
  data: T | null;
  error: string | null;
  loading: boolean;
  reload: () => void;
} {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(path !== null);
  const [nonce, setNonce] = useState(0);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    if (path === null) {
      setLoading(false);
      return;
    }

    const controller = new AbortController();
    setLoading(true);
    setError(null);

    api
      .get<T>(path, controller.signal)
      .then((result) => {
        setData(result);
        setError(null);
      })
      .catch((caught: unknown) => {
        // An aborted request is a navigation, not a failure.
        if (caught instanceof DOMException && caught.name === "AbortError") return;
        if (caught instanceof ApiError && caught.isUnauthenticated) return;
        setError(caught instanceof Error ? caught.message : "Something went wrong.");
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });

    return () => controller.abort();
  }, [path, nonce]);

  return { data, error, loading, reload };
}
