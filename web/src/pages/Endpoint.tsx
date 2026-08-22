import { useState } from "react";
import { useApi } from "../lib/useApi";
import type { SystemStatus } from "../lib/api";
import { Card, CodeBlock, ErrorNotice, InfoNotice, KeyValueRow, PageHeader, Spinner } from "../components/ui";

// Everything needed to point a client at this server. Every SDK needs the
// endpoint overridden and path-style forced, and boto3 additionally needs
// signature_version set or it falls back to SigV2 when presigning — so the
// snippets say so rather than leaving it to be discovered.

const clients = [
  { id: "awscli", label: "AWS CLI" },
  { id: "boto3", label: "Python (boto3)" },
  { id: "go", label: "Go (SDK v2)" },
  { id: "nodejs", label: "Node (SDK v3)" },
  { id: "env", label: "Environment" },
] as const;

export function EndpointPage() {
  const { data, error, loading, reload } = useApi<SystemStatus>("/api/system");
  const [client, setClient] = useState<string>("awscli");

  if (loading) return <Spinner label="Loading endpoint details" />;
  if (error) return <ErrorNotice message={error} onRetry={reload} />;
  if (!data) return null;

  const endpoint = data.endpoints.s3;
  const region = data.endpoints.region;

  return (
    <>
      <PageHeader
        title="Endpoint & SDKs"
        subtitle="How to point an S3 client at this server."
      />

      <Card className="mb-[16px] px-[18px] py-[4px]">
        <KeyValueRow label="Endpoint" value={endpoint} />
        <KeyValueRow label="Region" value={region} />
        <KeyValueRow label="Addressing" value={data.endpoints.virtualHostStyle ? "path or virtual-host" : "path style"} />
        <KeyValueRow label="Signature" value="AWS Signature Version 4" />
      </Card>

      <div className="mb-[16px]">
        <InfoNotice>
          <p className="m-0 mb-[4px] font-semibold">Two settings every client needs</p>
          <p className="m-0">
            Override the endpoint, and force <span className="font-mono">path-style</span> addressing
            {data.endpoints.virtualHostStyle
              ? " (virtual-host style also works, since a domain is configured)"
              : " (virtual-host style needs a wildcard DNS record and certificate, which are not configured)"}
            . boto3 additionally needs <span className="font-mono">signature_version="s3v4"</span>, or it
            silently falls back to the deprecated SigV2 when presigning, which this server refuses.
          </p>
        </InfoNotice>
      </div>

      <Card padded>
        <div className="mb-[12px] flex flex-wrap gap-[4px]">
          {clients.map((entry) => (
            <button
              key={entry.id}
              onClick={() => setClient(entry.id)}
              className={`rounded-[8px] px-[11px] py-[6px] text-[12.5px] font-medium ${
                client === entry.id ? "bg-accent text-on-accent" : "text-ink-muted hover:bg-inset hover:text-ink"
              }`}
            >
              {entry.label}
            </button>
          ))}
        </div>

        <CodeBlock text={snippetFor(client, endpoint, region)} />

        <p className="mt-[12px] text-[12px] leading-[1.6] text-ink-faint">
          Replace the placeholders with a real key pair from the Access keys screen. Creating a key
          there returns these same snippets already filled in.
        </p>
      </Card>
    </>
  );
}

/**
 * Connection snippets with placeholder credentials.
 *
 * Real secrets are never shown here: this screen is reachable at any time, and
 * the secret exists only in the moment a key is created.
 */
function snippetFor(client: string, endpoint: string, region: string): string {
  const key = "YOUR_ACCESS_KEY_ID";
  const secret = "YOUR_SECRET_ACCESS_KEY";

  switch (client) {
    case "env":
      return [
        `export AWS_ACCESS_KEY_ID=${key}`,
        `export AWS_SECRET_ACCESS_KEY=${secret}`,
        `export AWS_DEFAULT_REGION=${region}`,
        `export AWS_ENDPOINT_URL=${endpoint}`,
      ].join("\n");

    case "boto3":
      return `import boto3
from botocore.config import Config

s3 = boto3.client(
    "s3",
    endpoint_url="${endpoint}",
    aws_access_key_id="${key}",
    aws_secret_access_key="${secret}",
    region_name="${region}",
    # Required: this server is SigV4 only, and botocore otherwise
    # falls back to SigV2 when presigning against a custom endpoint.
    config=Config(signature_version="s3v4",
                  s3={"addressing_style": "path"}),
)`;

    case "go":
      return `client := s3.New(s3.Options{
    Region:       "${region}",
    BaseEndpoint: aws.String("${endpoint}"),
    UsePathStyle: true,
    Credentials: credentials.NewStaticCredentialsProvider(
        "${key}", "${secret}", ""),
})`;

    case "nodejs":
      return `import { S3Client } from "@aws-sdk/client-s3";

const s3 = new S3Client({
  region: "${region}",
  endpoint: "${endpoint}",
  forcePathStyle: true,
  credentials: {
    accessKeyId: "${key}",
    secretAccessKey: "${secret}",
  },
});`;

    default:
      return `# ~/.aws/config
[profile pail]
region = ${region}
endpoint_url = ${endpoint}
s3 =
    addressing_style = path

# ~/.aws/credentials
[pail]
aws_access_key_id = ${key}
aws_secret_access_key = ${secret}

# then:
aws --profile pail s3 ls`;
  }
}
