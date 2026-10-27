import * as cdk from 'aws-cdk-lib';
import { Match, Template } from 'aws-cdk-lib/assertions';
import { ALARMS, API_PORT, CLIENT_IP_HEADER, MemberServiceStack, MemberServiceStackProps, ORIGIN_HEADER, SERVICE_VAULT_API_PORT, TASK_CPU, TASK_MEMORY_MIB } from '../lib/member-service-stack';

const DIGEST = 'sha256:' + 'a'.repeat(64);
const base: MemberServiceStackProps = {
  envName: 'test',
  image: `ghcr.io/vettid/member-service@${DIGEST}`,
  hostName: 'member.test.vettid.org',
  hostedZoneId: 'Z0000000000000000000',
  zoneName: 'test.vettid.org',
  mailFrom: 'no-reply@test.vettid.org',
  env: { account: '123456789012', region: 'us-east-1' },
};
const synth = (props: Partial<MemberServiceStackProps> = {}) => {
  const app = new cdk.App();
  return Template.fromStack(new MemberServiceStack(app, 'MS', { ...base, ...props }));
};
const t = synth();
const json = (x: unknown) => JSON.stringify(x);
const imported = (name: string) => ({ 'Fn::ImportValue': `ServiceVault-test-${name}` });

const container = () => {
  const td = Object.values<any>(t.findResources('AWS::ECS::TaskDefinition'))[0].Properties;
  expect(td.ContainerDefinitions).toHaveLength(1);
  return td.ContainerDefinitions[0];
};
const taskRoleStatements = (): any[] => {
  const td = Object.values<any>(t.findResources('AWS::ECS::TaskDefinition'))[0].Properties;
  const roleId = td.TaskRoleArn['Fn::GetAtt'][0];
  return Object.values<any>(t.findResources('AWS::IAM::Policy'))
    .filter((p) => p.Properties.Roles.some((r: any) => r.Ref === roleId))
    .flatMap((p) => p.Properties.PolicyDocument.Statement);
};

describe('network', () => {
  test('in the service vault VPC: no VPC, NAT gateway or load balancer of its own', () => {
    t.resourceCountIs('AWS::EC2::VPC', 0);
    t.resourceCountIs('AWS::EC2::NatGateway', 0);
    t.resourceCountIs('AWS::ElasticLoadBalancingV2::LoadBalancer', 0);
    const link = Object.values<any>(t.findResources('AWS::ApiGatewayV2::VpcLink'))[0].Properties;
    expect(link.SubnetIds).toHaveLength(2);
    for (const id of link.SubnetIds) expect(json(id)).toContain('ServiceVault-test-PublicSubnetIds');
  });

  test('the task admits only the VPC link, on its port; nothing from the internet', () => {
    const sgs = Object.values<any>(t.findResources('AWS::EC2::SecurityGroup'));
    const ingress = [
      ...sgs.flatMap((sg) => sg.Properties.SecurityGroupIngress ?? []),
      ...Object.values<any>(t.findResources('AWS::EC2::SecurityGroupIngress')).map((r) => r.Properties),
    ];
    for (const r of ingress) {
      expect(r.CidrIp).toBeUndefined();
      expect(r.CidrIpv6).toBeUndefined();
    }
    const taskIngress = ingress.filter((r) => json(r.GroupId).includes('TaskSg'));
    expect(taskIngress).toHaveLength(1);
    expect(taskIngress[0]).toMatchObject({ FromPort: API_PORT, ToPort: API_PORT });
    expect(json(taskIngress[0].SourceSecurityGroupId)).toContain('VpcLinkSg');
  });

  test('the task calls out on 443 and the service vault API only', () => {
    const sgs = t.findResources('AWS::EC2::SecurityGroup');
    const taskSg = Object.entries<any>(sgs).find(([id]) => id.startsWith('TaskSg'))![1].Properties;
    const egress = [
      ...(taskSg.SecurityGroupEgress ?? []),
      ...Object.values<any>(t.findResources('AWS::EC2::SecurityGroupEgress'))
        .map((r) => r.Properties)
        .filter((r) => json(r.GroupId).includes('TaskSg')),
    ];
    expect(egress.map((r) => r.FromPort).sort()).toEqual([443, SERVICE_VAULT_API_PORT]);
    expect(egress.find((r) => r.FromPort === SERVICE_VAULT_API_PORT).DestinationSecurityGroupId).toEqual(imported('SecurityGroupId'));
  });

  test('the service vault admits the task on its API port', () => {
    t.hasResourceProperties('AWS::EC2::SecurityGroupIngress', {
      GroupId: imported('SecurityGroupId'),
      FromPort: SERVICE_VAULT_API_PORT,
      ToPort: SERVICE_VAULT_API_PORT,
      SourceSecurityGroupId: { 'Fn::GetAtt': [Match.stringLikeRegexp('^TaskSg'), 'GroupId'] },
    });
  });
});

describe('data', () => {
  test('one table, pk/sk, TTL on exp, PITR, deletion protection, retained', () => {
    t.resourceCountIs('AWS::DynamoDB::Table', 1);
    t.hasResource('AWS::DynamoDB::Table', {
      DeletionPolicy: 'Retain',
      Properties: Match.objectLike({
        TableName: 'vettid-member-service-test',
        KeySchema: [
          { AttributeName: 'pk', KeyType: 'HASH' },
          { AttributeName: 'sk', KeyType: 'RANGE' },
        ],
        TimeToLiveSpecification: { AttributeName: 'exp', Enabled: true },
        PointInTimeRecoverySpecification: { PointInTimeRecoveryEnabled: true },
        DeletionProtectionEnabled: true,
        BillingMode: 'PAY_PER_REQUEST',
      }),
    });
  });
});

describe('task', () => {
  test('one ARM64 Spot task, 0.25 vCPU / 512 MB, no exec, Cloud Map SRV', () => {
    t.hasResourceProperties('AWS::ECS::TaskDefinition', {
      Cpu: String(TASK_CPU),
      Memory: String(TASK_MEMORY_MIB),
      RuntimePlatform: { CpuArchitecture: 'ARM64', OperatingSystemFamily: 'LINUX' },
    });
    t.hasResourceProperties('AWS::ECS::Service', {
      DesiredCount: 1,
      CapacityProviderStrategy: [{ CapacityProvider: 'FARGATE_SPOT', Weight: 1 }],
      EnableExecuteCommand: false,
      NetworkConfiguration: { AwsvpcConfiguration: Match.objectLike({ AssignPublicIp: 'ENABLED' }) },
    });
    t.hasResourceProperties('AWS::ServiceDiscovery::Service', {
      Name: 'member-api',
      NamespaceId: imported('NamespaceId'),
      DnsConfig: Match.objectLike({ DnsRecords: [{ TTL: 10, Type: 'SRV' }] }),
    });
  });

  test('image by digest, read-only root, health check by the binary', () => {
    const c = container();
    expect(c.Image).toBe(base.image);
    expect(c.ReadonlyRootFilesystem).toBe(true);
    expect(c.HealthCheck.Command).toEqual(['CMD', '/member-api', 'healthcheck']);
    expect(c.PortMappings).toEqual([expect.objectContaining({ ContainerPort: API_PORT })]);
  });

  test('configuration: the service vault from its exports; secrets only as secrets', () => {
    const c = container();
    const env = Object.fromEntries(c.Environment.map((e: any) => [e.Name, e.Value]));
    expect(env).toMatchObject({
      MEMBER_API_ADDR: `:${API_PORT}`,
      SERVICE_VAULT_URL: imported('ApiUrl'),
      MAIL_FROM: base.mailFrom,
      CLIENT_IP_HEADER,
    });
    expect(env.MAIL_DIR).toBeUndefined();
    expect(env.DYNAMODB_ENDPOINT).toBeUndefined();
    const secrets = Object.fromEntries(c.Secrets.map((s: any) => [s.Name, s.ValueFrom]));
    expect(Object.keys(secrets).sort()).toEqual(['MEMBER_API_PEPPER', 'ORIGIN_VERIFY_SECRET', 'SERVICE_VAULT_API_KEY', 'SERVICE_VAULT_PIN', 'WEBHOOK_SECRET']);
    expect(secrets.WEBHOOK_SECRET).toEqual(imported('WebhookSecretArn'));
    expect(json(secrets.SERVICE_VAULT_PIN)).toContain('ServiceVault-test-TlsPinParameterName');
    for (const name of Object.keys(secrets)) expect(env[name]).toBeUndefined();
  });

  test('task role: the table, SES from the sender only; no wildcard beyond SES', () => {
    const st = taskRoleStatements();
    const ses = st.find((s) => s.Sid === 'SendCodes');
    expect(ses).toMatchObject({ Action: 'ses:SendEmail', Resource: '*', Condition: { StringEquals: { 'ses:FromAddress': base.mailFrom } } });
    for (const s of st.filter((x) => x.Sid !== 'SendCodes')) {
      expect([s.Resource].flat()).not.toContain('*');
      for (const a of [s.Action].flat()) expect(a).toMatch(/^dynamodb:/);
    }
  });

  test('the generated secrets are retained, except the origin header (rotatable with a redeploy)', () => {
    const secrets = t.findResources('AWS::SecretsManager::Secret');
    const byName = Object.fromEntries(Object.values<any>(secrets).map((s) => [s.Properties.Name, s]));
    expect(Object.keys(byName).sort()).toEqual([
      'vettid-member-service-test/origin-verify',
      'vettid-member-service-test/pepper',
      'vettid-member-service-test/service-vault-api-key',
    ]);
    expect(byName['vettid-member-service-test/pepper'].DeletionPolicy).toBe('Retain');
    expect(byName['vettid-member-service-test/service-vault-api-key'].DeletionPolicy).toBe('Retain');
  });
});

describe('API and site', () => {
  test('HTTP API: everything to the task through the VPC link, throttled, metadata-only access log', () => {
    t.hasResourceProperties('AWS::ApiGatewayV2::Integration', {
      IntegrationType: 'HTTP_PROXY',
      ConnectionType: 'VPC_LINK',
      IntegrationUri: { 'Fn::GetAtt': [Match.stringLikeRegexp('^ServiceCloudmapService'), 'Arn'] },
    });
    t.hasResourceProperties('AWS::ApiGatewayV2::Route', { RouteKey: '$default' });
    const stage = Object.values<any>(t.findResources('AWS::ApiGatewayV2::Stage'))[0].Properties;
    expect(stage.DefaultRouteSettings).toEqual({ ThrottlingRateLimit: 50, ThrottlingBurstLimit: 100 });
    const format = stage.AccessLogSettings.Format;
    for (const banned of ['header', 'cookie', 'sourceIp', 'body']) expect(format).not.toContain(banned);
  });

  test('CloudFront: /api/* to the HTTP API with the origin-verify header and the viewer address', () => {
    const dist = Object.values<any>(t.findResources('AWS::CloudFront::Distribution'))[0].Properties.DistributionConfig;
    expect(dist.Aliases).toEqual([base.hostName]);
    const api = dist.CacheBehaviors.find((b: any) => b.PathPattern === '/api/*');
    expect(api).toBeDefined();
    const origin = dist.Origins.find((o: any) => o.Id === api.TargetOriginId);
    expect(json(origin.DomainName)).toContain('execute-api');
    expect(origin.OriginCustomHeaders.map((h: any) => h.HeaderName)).toEqual([ORIGIN_HEADER]);
    expect(json(origin.OriginCustomHeaders[0].HeaderValue)).toContain('resolve:secretsmanager');
    t.hasResourceProperties('AWS::CloudFront::OriginRequestPolicy', {
      OriginRequestPolicyConfig: Match.objectLike({ HeadersConfig: Match.objectLike({ Headers: Match.arrayWith([CLIENT_IP_HEADER]) }) }),
    });
  });

  test('the site has its CSP (StaticSite) and a branded 404', () => {
    const policy = Object.values<any>(t.findResources('AWS::CloudFront::ResponseHeadersPolicy'))[0].Properties.ResponseHeadersPolicyConfig;
    const csp = policy.SecurityHeadersConfig.ContentSecurityPolicy.ContentSecurityPolicy;
    expect(csp).toContain("script-src 'self'");
    expect(csp).toContain("require-trusted-types-for 'script'");
    const fn = Object.values<any>(t.findResources('AWS::CloudFront::Function'))[0].Properties.FunctionCode;
    expect(fn).toContain('"/signin/index.html":1');
    expect(fn).toContain('NOT_FOUND');
  });
});

describe('alarms', () => {
  test('one alarm per marked log line, on the API log group', () => {
    for (const name of Object.keys(ALARMS)) {
      t.hasResourceProperties('AWS::Logs::MetricFilter', {
        FilterPattern: `{ $.alarm = "${name}" }`,
        MetricTransformations: [Match.objectLike({ MetricName: name, MetricNamespace: 'VettID/MemberService/test' })],
      });
      t.hasResourceProperties('AWS::CloudWatch::Alarm', { AlarmName: `vettid-member-service-test-${name}`, Threshold: 1 });
    }
  });

  test('with a topic the alarms notify it', () => {
    const topic = 'arn:aws:sns:us-east-1:123456789012:alerts';
    const alarms = Object.values<any>(synth({ alarmTopicArn: topic }).findResources('AWS::CloudWatch::Alarm'));
    for (const a of alarms) expect(a.Properties.AlarmActions).toEqual([topic]);
  });
});

describe('validation', () => {
  test.each([
    ['a tag instead of a digest', { image: 'ghcr.io/vettid/member-service:main' }],
    ['a bad env name', { envName: 'Prod' }],
    ['a host outside the zone', { hostName: 'member.example.org' }],
    ['a bad sender', { mailFrom: 'nobody' }],
    ['another region', { env: { account: '123456789012', region: 'eu-west-1' } }],
  ])('refuses %s', (_name, props) => {
    expect(() => synth(props as Partial<MemberServiceStackProps>)).toThrow();
  });
});
