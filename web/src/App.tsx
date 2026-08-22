import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { Shell } from "./components/Shell";
import { SignInPage } from "./pages/SignIn";
import { DashboardPage } from "./pages/Dashboard";
import { BucketsPage } from "./pages/Buckets";
import { ObjectsPage } from "./pages/Objects";
import { CredentialsPage } from "./pages/Credentials";
import { UsersPage } from "./pages/Users";
import { useSession } from "./lib/session";
import { Spinner } from "./components/ui";

export function App() {
  const { user, loading } = useSession();
  const location = useLocation();

  if (loading) {
    return (
      <div className="flex h-full items-center justify-center">
        <Spinner label="Loading console" />
      </div>
    );
  }

  if (!user) {
    // Everything funnels to sign-in, but the intended destination is kept so a
    // bookmarked deep link survives the round trip through email.
    return (
      <Routes>
        <Route path="/sign-in" element={<SignInPage />} />
        <Route
          path="*"
          element={<Navigate to="/sign-in" replace state={{ from: location.pathname }} />}
        />
      </Routes>
    );
  }

  return (
    <Shell>
      <Routes>
        <Route path="/" element={<DashboardPage />} />
        <Route path="/buckets" element={<BucketsPage />} />
        {/* The bucket's contents live under a wildcard so a prefix with
            slashes is a real URL, and the browser's back button works through
            a folder hierarchy. */}
        <Route path="/buckets/:bucket/*" element={<ObjectsPage />} />
        <Route path="/credentials" element={user.isAdmin ? <CredentialsPage /> : <Navigate to="/" replace />} />
        <Route path="/users" element={user.isAdmin ? <UsersPage /> : <Navigate to="/" replace />} />
        <Route path="/sign-in" element={<Navigate to="/" replace />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </Shell>
  );
}
