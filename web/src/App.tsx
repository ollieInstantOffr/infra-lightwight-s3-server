import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { Shell } from "./components/Shell";
import { SignInPage } from "./pages/SignIn";
import { DashboardPage } from "./pages/Dashboard";
import { BucketsPage } from "./pages/Buckets";
import { ObjectsPage } from "./pages/Objects";
import { CredentialsPage } from "./pages/Credentials";
import { EndpointPage } from "./pages/Endpoint";
import { UsersPage } from "./pages/Users";
import { SystemPage } from "./pages/System";
import { AuditPage_ } from "./pages/Audit";
import { AccountPage } from "./pages/Account";
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
    // Everything funnels to sign-in, keeping the intended destination so a
    // bookmarked deep link survives the round trip through email.
    return (
      <Routes>
        <Route path="/sign-in" element={<SignInPage />} />
        <Route path="*" element={<Navigate to="/sign-in" replace state={{ from: location.pathname }} />} />
      </Routes>
    );
  }

  return (
    <Shell>
      <Routes>
        <Route path="/" element={<DashboardPage />} />
        <Route path="/buckets" element={<BucketsPage />} />
        {/* The prefix lives under a wildcard so a folder path is a real URL and
            the back button walks the hierarchy. */}
        <Route path="/buckets/:bucket/*" element={<ObjectsPage />} />
        <Route path="/endpoint" element={<EndpointPage />} />
        <Route path="/system" element={<SystemPage />} />
        <Route path="/account" element={<AccountPage />} />
        {/* Admin-only screens redirect rather than 404, since a member
            following a shared link should land somewhere useful. */}
        <Route path="/keys" element={user.isAdmin ? <CredentialsPage /> : <Navigate to="/" replace />} />
        <Route path="/users" element={user.isAdmin ? <UsersPage /> : <Navigate to="/" replace />} />
        <Route path="/audit" element={user.isAdmin ? <AuditPage_ /> : <Navigate to="/" replace />} />
        <Route path="/sign-in" element={<Navigate to="/" replace />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </Shell>
  );
}
