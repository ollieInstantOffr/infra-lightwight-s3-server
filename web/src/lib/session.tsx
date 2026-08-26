import { createContext, useCallback, useContext, useEffect, useState } from "react";
import type { ReactNode } from "react";
import { ApiError, api, type CurrentUser } from "./api";

// Who is signed in, resolved once at startup.
//
// The app cannot render anything useful before this is known — the navigation
// differs for admins, and every screen would flash unauthenticated content
// otherwise — so it blocks on the first check and nothing else.

type SessionState = {
  user: CurrentUser | null;
  loading: boolean;
  refresh: () => Promise<void>;
  signOut: () => Promise<void>;
};

const SessionContext = createContext<SessionState | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<CurrentUser | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
    try {
      setUser(await api.get<CurrentUser>("/api/auth/me"));
    } catch (error) {
      // A 401 here is the ordinary "not signed in" case, not a failure.
      if (error instanceof ApiError && error.isUnauthenticated) {
        setUser(null);
      } else {
        setUser(null);
      }
    } finally {
      setLoading(false);
    }
  }, []);

  const signOut = useCallback(async () => {
    try {
      await api.post("/api/auth/logout");
    } finally {
      // The cookie is gone either way; keeping stale state would leave the app
      // showing a signed-in shell that fails every request.
      setUser(null);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  return (
    <SessionContext.Provider value={{ user, loading, refresh, signOut }}>
      {children}
    </SessionContext.Provider>
  );
}

export function useSession(): SessionState {
  const context = useContext(SessionContext);
  if (!context) {
    throw new Error("useSession must be used inside a SessionProvider");
  }
  return context;
}
