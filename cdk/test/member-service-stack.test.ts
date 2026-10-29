import * as cdk from 'aws-cdk-lib';
import { Match, Template } from 'aws-cdk-lib/assertions';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { ALARMS, API_PORT, ERROR_THRESHOLD, EXPECTED_STOP_CODES, CLIENT_IP_HEADER, WEBHOOK_HEADERS, MemberServiceStack, MemberServiceStackProps, ORIGIN_HEADER, ORIGIN_SERIAL_HEADER, SERVICE_VAULT_API_PORT, TASK_CPU, TASK_MEMORY_MIB } from '../lib/member-service-stack';

const DIGEST = 'sha256:' + 'a'.repeat(64);
const base: MemberServiceStackProps = {
  envName: 'test',
  image: `ghcr.io/vettid/member-service@${DIGEST}`,
  hostName: 'member.test.vettid.org',
  hostedZoneId: 'Z0000000000000000000',
  zoneName: 'test.vettid.org',
  mailFrom: 'no-reply@test.vettid.org',
  alarmTopicArn: 'arn:aws:sns:us-east-1:123456789012:alerts',
  env: { account: '123456789012', region: 'us-east-1' },
};
const synth = (props: Partial<MemberServiceStackProps> = {}) => {
  const app = new cdk.App();
  return Template.fromStack(new MemberServiceStack(app, 'MS', { ...base, ...props }));
};
const t = synth();
const json = (x: unknown) => JSON.stringify(x);
const PARAMS: Record<string, string> = {
  PublicSubnetIds: 'public-subnet-ids', VpcId: 'vpc-id', SecurityGroupId: 'security-group-id', NamespaceId: 'namespace-id',
  ApiUrl: 'api-url', WebhookSecretArn: 'webhook-secret-arn',
};
/** The reference to a service vault value: a deploy-time SSM parameter, never an import. */
const imported = (name: string) => {
  const path = `/vettid/service-vault/test/${PARAMS[name]}`;
  const [id] = Object.entries<any>(t.toJSON().Parameters).find(([, p]) => p.Default === path) ?? [];
  if (!id) throw new Error(`no parameter for ${path}`);
  return { Ref: id };
};

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

describe('service vault values', () => {
  test('read from SSM at deploy time; nothing imported, so the service vault stack stays free to change', () => {
    expect(json(t.toJSON())).not.toContain('Fn::ImportValue');
    for (const p of Object.values<any>(t.toJSON().Parameters).filter((p) => String(p.Default).startsWith('/vettid/service-vault/'))) {
      expect(p.Type).toBe('AWS::SSM::Parameter::Value<String>');
    }
  });
});

describe('network', () => {
  test('in the service vault VPC: no VPC, NAT gateway or load balancer of its own', () => {
    t.resourceCountIs('AWS::EC2::VPC', 0);
    t.resourceCountIs('AWS::EC2::NatGateway', 0);
    t.resourceCountIs('AWS::ElasticLoadBalancingV2::LoadBalancer', 0);
    const link = Object.values<any>(t.findResources('AWS::ApiGatewayV2::VpcLink'))[0].Properties;
    expect(link.SubnetIds).toHaveLength(2);
    for (const id of link.SubnetIds) expect(json(id)).toContain(imported('PublicSubnetIds').Ref);
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
    expect(c.LinuxParameters.Capabilities.Drop).toEqual(['ALL']);
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
    expect(Object.keys(secrets).sort()).toEqual(['MEMBER_API_PEPPER', 'ORIGIN_VERIFY_SECRET', 'ORIGIN_VERIFY_SECRET_PREVIOUS', 'SERVICE_VAULT_API_KEY', 'SERVICE_VAULT_PIN', 'WEBHOOK_SECRET']);
    expect(secrets.WEBHOOK_SECRET).toEqual(imported('WebhookSecretArn'));
    expect(json(secrets.SERVICE_VAULT_PIN)).toContain(':parameter/vettid/service-vault/test/tls-pin');
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

  test('task role: exactly the item calls the API makes, on the table itself', () => {
    const tableId = Object.keys(t.findResources('AWS::DynamoDB::Table'))[0];
    const ddb = taskRoleStatements().filter((s) => [s.Action].flat().some((a: string) => a.startsWith('dynamodb:')));
    expect(ddb).toHaveLength(1);
    expect([ddb[0].Action].flat().sort()).toEqual(['dynamodb:DeleteItem', 'dynamodb:GetItem', 'dynamodb:PutItem', 'dynamodb:Query', 'dynamodb:UpdateItem']);
    expect([ddb[0].Resource].flat()).toEqual([{ 'Fn::GetAtt': [tableId, 'Arn'] }]);
  });

  test('the generated secrets: the API key and pepper retained; nothing retained has a fixed name', () => {
    const secrets = Object.entries<any>(t.findResources('AWS::SecretsManager::Secret'));
    expect(secrets.map(([id]) => id.replace(/[0-9A-F]{8}$/, '')).sort()).toEqual(['OriginVerify', 'OriginVerifyPrevious', 'Pepper', 'ServiceVaultApiKey']);
    for (const [id, s] of secrets) {
      expect(s.Properties.Name).toBeUndefined();
      if (!id.startsWith('OriginVerify')) expect(s.DeletionPolicy).toBe('Retain');
    }
    for (const type of ['AWS::DynamoDB::Table', 'AWS::Logs::LogGroup']) {
      for (const r of Object.values<any>(t.findResources(type))) {
        expect(r.Properties.TableName ?? r.Properties.LogGroupName).toBeUndefined();
      }
    }
  });

  test('the execution role reads exactly the six task secrets', () => {
    const td = Object.values<any>(t.findResources('AWS::ECS::TaskDefinition'))[0].Properties;
    const roleId = td.ExecutionRoleArn['Fn::GetAtt'][0];
    const st = Object.values<any>(t.findResources('AWS::IAM::Policy'))
      .filter((p) => p.Properties.Roles.some((r: any) => r.Ref === roleId))
      .flatMap((p) => p.Properties.PolicyDocument.Statement);
    const sm = st.filter((s) => [s.Action].flat().includes('secretsmanager:GetSecretValue'));
    const smResources = sm.flatMap((s) => [s.Resource].flat());
    expect(smResources).toHaveLength(5);
    expect(smResources).toContainEqual(imported('WebhookSecretArn'));
    const ssmSt = st.filter((s) => [s.Action].flat().some((a: string) => a.startsWith('ssm:')));
    expect(ssmSt).toHaveLength(1);
    expect(json(ssmSt[0].Resource)).toContain(':parameter/vettid/service-vault/test/tls-pin');
    for (const s of st) expect([s.Resource].flat()).not.toContain('*');
  });

  test('every variable the member API reads is set, as a value or a secret, and none it does not', () => {
    const main = readFileSync(join(__dirname, '..', '..', 'api', 'cmd', 'member-api', 'main.go'), 'utf8');
    const read = new Set([...main.matchAll(/getenv\("([A-Z_]+)"\)/g)].map((m) => m[1]));
    const c = container();
    const set = [...c.Environment.map((e: any) => e.Name), ...c.Secrets.map((s: any) => s.Name)].filter((n) => n !== 'AWS_REGION');
    for (const name of set) expect(read).toContain(name);
    for (const name of ['MEMBER_API_ADDR', 'SERVICE_VAULT_URL', 'SERVICE_VAULT_API_KEY', 'SERVICE_VAULT_PIN', 'WEBHOOK_SECRET', 'MEMBER_API_PEPPER', 'DYNAMODB_TABLE', 'MAIL_FROM', 'CLIENT_IP_HEADER', 'ORIGIN_VERIFY_SECRET']) {
      expect(set).toContain(name);
    }
  });

  test('an ECR image is pulled by digest from its repository', () => {
    const ecrImage = `123456789012.dkr.ecr.us-east-1.amazonaws.com/member-api@${DIGEST}`;
    const td = Object.values<any>(synth({ image: ecrImage }).findResources('AWS::ECS::TaskDefinition'))[0].Properties;
    expect(json(td.ContainerDefinitions[0].Image)).toContain(`member-api@${DIGEST}`);
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

  test('originSecretSerial adds the serial header, so a deploy reads the origin secret again', () => {
    const origin = (tt: Template) => {
      const dist = Object.values<any>(tt.findResources('AWS::CloudFront::Distribution'))[0].Properties.DistributionConfig;
      const api = dist.CacheBehaviors.find((b: any) => b.PathPattern === '/api/*');
      return dist.Origins.find((o: any) => o.Id === api.TargetOriginId);
    };
    const headers = origin(synth({ originSecretSerial: '20261028-1900' })).OriginCustomHeaders;
    expect(headers.map((h: any) => h.HeaderName)).toEqual([ORIGIN_HEADER, ORIGIN_SERIAL_HEADER]);
    expect(headers[1].HeaderValue).toBe('20261028-1900');
    expect(json(headers[0].HeaderValue)).toContain('resolve:secretsmanager');
    for (const bad of ['a b', 'x'.repeat(65), '{{resolve:x}}']) {
      expect(() => synth({ originSecretSerial: bad })).toThrow(/originSecretSerial/);
    }
  });

  test('CloudFront forwards the webhook signature headers to /api/* (else every webhook is a 401)', () => {
    const policy = Object.values<any>(t.findResources('AWS::CloudFront::OriginRequestPolicy'))[0].Properties.OriginRequestPolicyConfig;
    const headers: string[] = policy.HeadersConfig.Headers;
    for (const h of [...WEBHOOK_HEADERS, 'Content-Type', CLIENT_IP_HEADER]) expect(headers).toContain(h);
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

  test('site: hashed JS and CSS cache a year as immutable and stay across deploys; pages go out after them', () => {
    const resources = t.findResources('Custom::CDKBucketDeployment');
    const by = (cc: string) => Object.entries<any>(resources).filter(([, r]) => json(r.Properties.SystemMetadata ?? {}).includes(cc));
    const [[immutableId, immutable]] = by('max-age=31536000, immutable');
    const [[, assets]] = by('max-age=604800').filter(([, r]) => r.Properties.Prune === false);
    const [[, pages]] = by('must-revalidate');
    const globs = ['js', 'css'].map((ext) => `*.${'[0-9a-f]'.repeat(12)}.${ext}`);
    expect(immutable.Properties.Exclude).toEqual(['*']);
    expect(immutable.Properties.Include).toEqual(globs);
    // Never pruned: a page loaded before a deploy still finds its files.
    expect(immutable.Properties.Prune).toBe(false);
    // The weekly pass neither uploads nor prunes them.
    expect(assets.Properties.Exclude).toEqual(expect.arrayContaining(globs));
    expect(pages.Properties.Include).toEqual(expect.arrayContaining(['*.html']));
    expect(pages.DependsOn).toEqual(expect.arrayContaining([immutableId]));
    // The weekly pass: uploaded before the pages without pruning, pruned
    // by a second pass after them.
    const pagesId = Object.keys(resources).find((id) => resources[id] === pages)!;
    const weekly = by('max-age=604800');
    expect(weekly).toHaveLength(2);
    const [[assetsId, upload], [, prune]] = weekly.sort(([, a]) => (a.Properties.Prune === false ? -1 : 1));
    expect(upload.Properties.Prune).toBe(false);
    expect(prune.Properties.Prune).not.toBe(false);
    expect(prune.Properties.Exclude).toEqual(upload.Properties.Exclude);
    expect(pages.DependsOn).toEqual(expect.arrayContaining([immutableId, assetsId]));
    expect(prune.DependsOn).toEqual(expect.arrayContaining([pagesId]));
  });

  test('site: the 404 check passes hashed versions of the site\'s files to S3, and only those', () => {
    const code = Object.values<any>(t.findResources('AWS::CloudFront::Function'))[0].Properties.FunctionCode as string;
    const handler = new Function(`${code}; return handler;`)();
    const status = (uri: string) => {
      const out = handler({ request: { uri, cookies: {} } });
      return out.statusCode ?? 'pass';
    };
    const connect = /"(\/js\/connect\.[0-9a-f]{12}\.js)":1/.exec(code)![1];
    expect(status(connect)).toBe('pass'); // this build's
    expect(status('/js/connect.0123456789ab.js')).toBe('pass'); // an earlier deploy's
    expect(status('/js/vendor/qrcode-generator.0123456789ab.js')).toBe('pass');
    expect(status('/styles.0123456789ab.css')).toBe('pass');
    expect(status('/js/x.000000000000.js')).toBe(404); // not a version of a site file
    expect(status('/js/connect.0123456789ab.css')).toBe(404);
    expect(status('/js/connect.js')).toBe(404); // the unhashed name is not published
    expect(status('/connect/')).toBe('pass');
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

  test('down: no running task for 10 minutes', () => {
    t.hasResourceProperties('AWS::CloudWatch::Alarm', {
      AlarmName: 'vettid-member-service-test-down',
      MetricName: 'CPUUtilization',
      Namespace: 'AWS/ECS',
      Statistic: 'SampleCount',
      ComparisonOperator: 'LessThanThreshold',
      TreatMissingData: 'breaching',
    });
    expect(Object.keys(t.findResources('AWS::CloudWatch::Alarm'))).toHaveLength(Object.keys(ALARMS).length + 4); // + errors, task_stopped, deploy_failed, down
  });

  test('task_stopped and deploy_failed: one rule sends the cluster\'s ECS events to a log group', () => {
    const rules = Object.values<any>(t.findResources('AWS::Events::Rule'));
    expect(rules).toHaveLength(1);
    const p = rules[0].Properties.EventPattern;
    expect(p.source).toEqual(['aws.ecs']);
    expect(p['detail-type']).toEqual(['ECS Task State Change', 'ECS Deployment State Change']);
    const res = JSON.stringify(p.resources);
    expect(res).toContain(':task/vettid-member-service-test/');
    expect(res).toContain(':service/vettid-member-service-test/');
    const filters = Object.values<any>(t.findResources('AWS::Logs::MetricFilter')).map((f: any) => f.Properties);
    const stopped = filters.find((f: any) => f.MetricTransformations[0].MetricName === 'task_stopped').FilterPattern;
    expect(stopped).toContain('$.detail.lastStatus = "STOPPED"');
    for (const c of EXPECTED_STOP_CODES) expect(stopped).toContain(`$.detail.stopCode != "${c}"`);
    expect(stopped).toContain('$.detail.stoppedReason = "Task failed*"');
    expect(filters.find((f: any) => f.MetricTransformations[0].MetricName === 'deploy_failed').FilterPattern).toContain('SERVICE_DEPLOYMENT_FAILED');
    for (const n of ['task_stopped', 'deploy_failed']) t.hasResourceProperties('AWS::CloudWatch::Alarm', { AlarmName: `vettid-member-service-test-${n}`, Threshold: 1 });
  });

  test('errors: ERROR lines without an alarm of their own, over 15 minutes', () => {
    const f = Object.values<any>(t.findResources('AWS::Logs::MetricFilter')).map((x: any) => x.Properties)
      .find((x: any) => x.MetricTransformations[0].MetricName === 'errors');
    expect(f.FilterPattern).toBe('{ ($.level = "ERROR") && ($.alarm NOT EXISTS) }');
    t.hasResourceProperties('AWS::CloudWatch::Alarm', { AlarmName: 'vettid-member-service-test-errors', Threshold: ERROR_THRESHOLD, Period: 900 });
  });

  test('every alarm notifies the topic', () => {
    for (const a of Object.values<any>(t.findResources('AWS::CloudWatch::Alarm'))) {
      expect(a.Properties.AlarmActions).toEqual([base.alarmTopicArn]);
    }
  });

  test('with an address instead, a topic of its own with that subscription', () => {
    const t2 = synth({ alarmTopicArn: undefined, alarmEmail: 'ops@example.org' });
    t2.hasResourceProperties('AWS::SNS::Subscription', { Protocol: 'email', Endpoint: 'ops@example.org' });
    const topicId = Object.keys(t2.findResources('AWS::SNS::Topic'))[0];
    for (const a of Object.values<any>(t2.findResources('AWS::CloudWatch::Alarm'))) {
      expect(a.Properties.AlarmActions).toEqual([{ Ref: topicId }]);
    }
  });
});

describe('validation', () => {
  test.each([
    ['a tag instead of a digest', { image: 'ghcr.io/vettid/member-service:main' }],
    ['a bad env name', { envName: 'Prod' }],
    ['a host outside the zone', { hostName: 'member.example.org' }],
    ['a bad sender', { mailFrom: 'nobody' }],
    ['another region', { env: { account: '123456789012', region: 'eu-west-1' } }],
    ['no alarm destination', { alarmTopicArn: undefined }],
    ['two alarm destinations', { alarmEmail: 'ops@example.org' }],
    ['a bad topic', { alarmTopicArn: 'alerts' }],
  ])('refuses %s', (_name, props) => {
    expect(() => synth(props as Partial<MemberServiceStackProps>)).toThrow();
  });
});
