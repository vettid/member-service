import * as cdk from 'aws-cdk-lib';
import * as apigwv2 from 'aws-cdk-lib/aws-apigatewayv2';
import * as integrations from 'aws-cdk-lib/aws-apigatewayv2-integrations';
import * as cloudwatch from 'aws-cdk-lib/aws-cloudwatch';
import * as cwactions from 'aws-cdk-lib/aws-cloudwatch-actions';
import * as dynamodb from 'aws-cdk-lib/aws-dynamodb';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as ecr from 'aws-cdk-lib/aws-ecr';
import * as ecs from 'aws-cdk-lib/aws-ecs';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as logs from 'aws-cdk-lib/aws-logs';
import * as route53 from 'aws-cdk-lib/aws-route53';
import * as secretsmanager from 'aws-cdk-lib/aws-secretsmanager';
import * as servicediscovery from 'aws-cdk-lib/aws-servicediscovery';
import * as sns from 'aws-cdk-lib/aws-sns';
import * as subs from 'aws-cdk-lib/aws-sns-subscriptions';
import * as ssm from 'aws-cdk-lib/aws-ssm';
import { Construct } from 'constructs';
import { join } from 'node:path';
import { StaticSite } from './static-site';

/** The port the member API listens on (MEMBER_API_ADDR). */
export const API_PORT = 8081;

/** The service vault's private API port (service-vault cdk API_PORT). */
export const SERVICE_VAULT_API_PORT = 8080;

/** The Cloud Map service the HTTP API's VPC link resolves the task with. */
export const DISCOVERY_NAME = 'member-api';

/** Task size (MEMBER-SERVICE-PLAN §9): 0.25 vCPU / 512 MB. */
export const TASK_CPU = 256;
export const TASK_MEMORY_MIB = 512;

/** The header CloudFront adds to every /api/* request (member-api OriginHeader). */
export const ORIGIN_HEADER = 'X-Origin-Verify';

/**
 * The service vault's webhook headers (service-vault sdk/go/webhook
 * HeaderSignature, HeaderEventID): CloudFront forwards only an allow list
 * to /api/*, and without these every webhook is refused (401).
 */
export const WEBHOOK_HEADERS = ['VettID-Signature', 'VettID-Event-Id'];

/** The header CloudFront sets with the viewer's address (CLIENT_IP_HEADER). */
export const CLIENT_IP_HEADER = 'CloudFront-Viewer-Address';

/** The site's files, relative to this directory's parent (cdk/). */
export const SITE_DIR = join(__dirname, '..', '..', 'web');

/** An image reference pinned by digest, with no tag: <repository>@sha256:<64 hex>. */
const DIGEST_IMAGE = /^([a-z0-9.-]+(?::[0-9]+)?\/[a-z0-9._\/-]+)@(sha256:[0-9a-f]{64})$/;
/** A private ECR repository URI: <account>.dkr.ecr.<region>.amazonaws.com/<name>. */
const ECR_REPO = /^([0-9]{12})\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com\/(.+)$/;

/** Log lines the member API marks for an alarm (`"alarm": <name>`). */
export const ALARMS = {
  /** Wrong codes across all flows reached FailedAlarm in an hour (session.go). */
  verify_failed: 'Wrong one-time codes reached the alarm level this hour (a guessing run)',
  /** A sign-in approved with a credential key the service vault does not recognise (signin.go). */
  key_changed: 'A member approved a sign-in with a changed credential key (reset, new phone, or account recovery by someone else)',
} as const;

export interface MemberServiceStackProps extends cdk.StackProps {
  /** Environment name (e.g. `prod`, `staging`): part of every resource name, and of the service vault's SSM path. */
  readonly envName: string;
  /** Container image pinned by digest, never by tag (ECR or GHCR). */
  readonly image: string;
  /** The site's host, e.g. member.staging.vettid.org. */
  readonly hostName: string;
  /** The hosted zone the host is in (its id and name, e.g. staging.vettid.org). */
  readonly hostedZoneId: string;
  readonly zoneName: string;
  /** SES sender of a verified domain identity (MAIL_FROM), e.g. no-reply@staging.vettid.org. */
  readonly mailFrom: string;
  /**
   * Where the service vault stack publishes its values in SSM (default
   * `/vettid/service-vault/<envName>`, service-vault cdk ssmPrefix). They
   * are read at deploy time, which never locks that stack's values, as a
   * CloudFormation import would.
   */
  readonly serviceVaultParams?: string;
  /**
   * Where the alarms go; one of the two is required, so that an alarm
   * always reaches a person. An existing SNS topic (staging: vettid.org's
   * vault alerts topic, /vettid-org/staging/vault/alerts-topic-arn), or an
   * address for a topic of this stack's own (the subscription must be
   * confirmed from the email AWS sends).
   */
  readonly alarmTopicArn?: string;
  readonly alarmEmail?: string;
}

/**
 * member.vettid.org (MEMBER-SERVICE-PLAN §9, row 2.3): the static site on
 * S3 and CloudFront, and the member API on one ARM64 Fargate Spot task in
 * the service vault's VPC.
 *
 *  - Site: vettid.org's StaticSite construct (a copy, lib/static-site.ts)
 *    with its CSP and headers. `/api/*` goes to an API Gateway HTTP API,
 *    with the origin-verify secret CloudFront adds; the member API refuses
 *    requests without it, so the public execute-api URL is of no use on
 *    its own, and the viewer's address (rate limits) cannot be forged.
 *  - API: the HTTP API reaches the task through a VPC link and Cloud Map
 *    (the service vault's private namespace): no load balancer. The task
 *    has a public IP for its outbound calls (no NAT gateway) and a
 *    security group that admits only the VPC link.
 *  - Service vault: the task is the client its private API admits (an
 *    ingress rule on the service vault's security group); the API key, the
 *    TLS pin and the webhook secret come from Secrets Manager and SSM.
 *    Webhooks arrive at https://<hostName>/api/webhooks/service-vault.
 *  - Data: one DynamoDB table (pk/sk, TTL on exp), retained.
 */
export class MemberServiceStack extends cdk.Stack {
  readonly table: dynamodb.Table;
  readonly securityGroup: ec2.SecurityGroup;
  readonly service: ecs.FargateService;
  readonly api: apigwv2.HttpApi;
  readonly site: StaticSite;

  constructor(scope: Construct, id: string, props: MemberServiceStackProps) {
    super(scope, id, props);
    const envName = props.envName;
    if (!/^[a-z][a-z0-9-]{0,15}$/.test(envName)) {
      throw new Error(`envName must be lowercase letters, digits and hyphens (got "${envName}")`);
    }
    const image = DIGEST_IMAGE.exec(props.image);
    if (!image) {
      throw new Error(`image must be <repository>@sha256:<digest> with no tag (got "${props.image}")`);
    }
    if (!/^[a-z0-9-]+(\.[a-z0-9-]+)+$/.test(props.zoneName) || !props.hostName.endsWith(`.${props.zoneName}`)) {
      throw new Error(`hostName must be a name in zoneName (got "${props.hostName}" in "${props.zoneName}")`);
    }
    if (!/^[^@\s]+@[a-z0-9.-]+\.[a-z]+$/.test(props.mailFrom)) {
      throw new Error(`mailFrom must be an address (got "${props.mailFrom}")`);
    }
    if (!cdk.Token.isUnresolved(this.region) && this.region !== 'us-east-1') {
      // The site's certificate is made in this stack, and CloudFront takes
      // certificates from us-east-1 only.
      throw new Error(`the stack must be in us-east-1 (got ${this.region})`);
    }
    if (!props.alarmTopicArn === !props.alarmEmail) {
      throw new Error('set exactly one of alarmTopicArn and alarmEmail: the alarms must reach a person');
    }
    if (props.alarmTopicArn && !/^arn:aws[a-z-]*:sns:[a-z0-9-]+:[0-9]{12}:[A-Za-z0-9_-]+$/.test(props.alarmTopicArn)) {
      throw new Error(`alarmTopicArn must be an SNS topic ARN (got "${props.alarmTopicArn}")`);
    }
    if (props.alarmEmail && !/^[^@\s]+@[a-z0-9.-]+\.[a-z]+$/.test(props.alarmEmail)) {
      throw new Error(`alarmEmail must be an address (got "${props.alarmEmail}")`);
    }
    const prefix = `vettid-member-service-${envName}`;
    const svPrefix = props.serviceVaultParams ?? `/vettid/service-vault/${envName}`;
    if (!/^\/[A-Za-z0-9_.\/-]+[^/]$/.test(svPrefix)) {
      throw new Error(`serviceVaultParams must be an SSM path such as /vettid/service-vault/staging (got "${svPrefix}")`);
    }
    const sv = (name: string) => ssm.StringParameter.valueForStringParameter(this, `${svPrefix}/${name}`);

    // ---- The service vault's network --------------------------------------
    const subnetIds = cdk.Fn.split(',', sv('public-subnet-ids'), 2);
    const vpc = ec2.Vpc.fromVpcAttributes(this, 'Vpc', {
      vpcId: sv('vpc-id'),
      // Only labels on the imported subnets: no lookup at synth.
      availabilityZones: [cdk.Fn.select(0, cdk.Fn.getAzs()), cdk.Fn.select(1, cdk.Fn.getAzs())],
      publicSubnetIds: [cdk.Fn.select(0, subnetIds), cdk.Fn.select(1, subnetIds)],
    });
    // Nothing here reads the imported subnets' route tables.
    for (const subnet of vpc.publicSubnets) cdk.Annotations.of(subnet).acknowledgeWarning('@aws-cdk/aws-ec2:noSubnetRouteTableId');
    const namespace = servicediscovery.PrivateDnsNamespace.fromPrivateDnsNamespaceAttributes(this, 'Namespace', {
      namespaceId: sv('namespace-id'),
      namespaceArn: sv('namespace-arn'),
      namespaceName: sv('namespace-name'),
    });

    // The VPC link's interfaces may reach the task on its port, and only
    // them; the task calls out on 443 (SES, DynamoDB's gateway, Secrets
    // Manager, SSM, logs, the image registry) and the service vault's API.
    const linkSg = new ec2.SecurityGroup(this, 'VpcLinkSg', {
      vpc,
      description: 'member API VPC link: to the task only',
      allowAllOutbound: false,
    });
    this.securityGroup = new ec2.SecurityGroup(this, 'TaskSg', {
      vpc,
      description: 'member API task: HTTPS and the service vault out; in from the VPC link',
      allowAllOutbound: false,
    });
    linkSg.addEgressRule(this.securityGroup, ec2.Port.tcp(API_PORT), 'to the member API');
    this.securityGroup.addIngressRule(linkSg, ec2.Port.tcp(API_PORT), 'from the VPC link');
    this.securityGroup.addEgressRule(ec2.Peer.anyIpv4(), ec2.Port.tcp(443), 'HTTPS out');
    const serviceVaultSg = sv('security-group-id');
    new ec2.CfnSecurityGroupEgress(this, 'ToServiceVault', {
      groupId: this.securityGroup.securityGroupId,
      ipProtocol: 'tcp',
      fromPort: SERVICE_VAULT_API_PORT,
      toPort: SERVICE_VAULT_API_PORT,
      destinationSecurityGroupId: serviceVaultSg,
      description: 'the service vault private API',
    });
    // The service vault's stack leaves this rule to its client: the member
    // API is the client its private API admits.
    new ec2.CfnSecurityGroupIngress(this, 'ServiceVaultFromApi', {
      groupId: serviceVaultSg,
      ipProtocol: 'tcp',
      fromPort: SERVICE_VAULT_API_PORT,
      toPort: SERVICE_VAULT_API_PORT,
      sourceSecurityGroupId: this.securityGroup.securityGroupId,
      description: 'private API from the member API task',
    });

    // Retained resources are named by CloudFormation, not by us: a first
    // create that rolls back keeps them, and fixed names would make the
    // retry fail with "already exists". The outputs name them.

    // ---- Data ---------------------------------------------------------------
    this.table = new dynamodb.Table(this, 'Table', {
      partitionKey: { name: 'pk', type: dynamodb.AttributeType.STRING },
      sortKey: { name: 'sk', type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      // Flows, sessions, counters and webhook ids carry their expiry here
      // (store.Dynamo; reads check it too, as TTL deletes lag).
      timeToLiveAttribute: 'exp',
      pointInTimeRecoverySpecification: { pointInTimeRecoveryEnabled: true },
      deletionProtection: true,
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });

    // ---- Secrets: generated here, never in code ------------------------------
    // The API key this application presents to the service vault. The
    // operator adds its SHA-256 to the service vault's api-key-hashes
    // secret (README "Deployment"); until then every call is refused.
    const apiKey = new secretsmanager.Secret(this, 'ServiceVaultApiKey', {
      description: 'API key the member API presents to the service vault (SERVICE_VAULT_API_KEY)',
      generateSecretString: { passwordLength: 48, excludePunctuation: true, includeSpace: false },
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });
    // The key of the hashes of addresses and codes: replacing it forgets
    // every open flow and rate-limit counter (sessions and the roster stay).
    const pepper = new secretsmanager.Secret(this, 'Pepper', {
      description: 'Key of the hashes of email addresses and codes (MEMBER_API_PEPPER)',
      generateSecretString: { passwordLength: 64, excludePunctuation: true, includeSpace: false },
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });
    const originSecret = new secretsmanager.Secret(this, 'OriginVerify', {
      description: `Header value CloudFront adds to /api/* requests (${ORIGIN_HEADER}); the member API refuses requests without it`,
      generateSecretString: { passwordLength: 48, excludePunctuation: true, includeSpace: false },
    });
    const webhookSecret = secretsmanager.Secret.fromSecretCompleteArn(this, 'WebhookSecret', sv('webhook-secret-arn'));
    const tlsPin = ssm.StringParameter.fromStringParameterAttributes(this, 'TlsPin', { parameterName: `${svPrefix}/tls-pin`, simpleName: false });

    // ---- Compute ------------------------------------------------------------
    const cluster = new ecs.Cluster(this, 'Cluster', {
      clusterName: prefix,
      vpc,
      containerInsightsV2: ecs.ContainerInsights.DISABLED,
      enableFargateCapacityProviders: true,
    });
    const taskDef = new ecs.FargateTaskDefinition(this, 'Task', {
      cpu: TASK_CPU,
      memoryLimitMiB: TASK_MEMORY_MIB,
      runtimePlatform: { cpuArchitecture: ecs.CpuArchitecture.ARM64, operatingSystemFamily: ecs.OperatingSystemFamily.LINUX },
    });
    this.table.grantReadWriteData(taskDef.taskRole);
    // ses:SendEmail stays on '*', as in vettid.org's stacks: scoped to the
    // domain identity, sends from the SES sandbox fail (the recipient's
    // identity is checked too).
    taskDef.taskRole.addToPrincipalPolicy(
      new iam.PolicyStatement({
        sid: 'SendCodes',
        actions: ['ses:SendEmail'],
        resources: ['*'],
        conditions: { StringEquals: { 'ses:FromAddress': props.mailFrom } },
      }),
    );

    const logGroup = new logs.LogGroup(this, 'Logs', {
      retention: logs.RetentionDays.ONE_MONTH,
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });

    const [, repository, digest] = image;
    const ecrRepo = ECR_REPO.exec(repository);
    const containerImage = ecrRepo
      ? ecs.ContainerImage.fromEcrRepository(
          ecr.Repository.fromRepositoryAttributes(this, 'Repository', {
            repositoryName: ecrRepo[3],
            repositoryArn: `arn:${this.partition}:ecr:${ecrRepo[2]}:${ecrRepo[1]}:repository/${ecrRepo[3]}`,
          }),
          digest,
        )
      : ecs.ContainerImage.fromRegistry(props.image);

    // No Linux capabilities: the binary needs none (it listens on an
    // unprivileged port as a non-root user).
    const linuxParameters = new ecs.LinuxParameters(this, 'Linux');
    linuxParameters.dropCapabilities(ecs.Capability.ALL);
    taskDef.addContainer('MemberApi', {
      image: containerImage,
      essential: true,
      linuxParameters,
      portMappings: [{ containerPort: API_PORT }],
      readonlyRootFilesystem: true,
      environment: {
        MEMBER_API_ADDR: `:${API_PORT}`,
        SERVICE_VAULT_URL: sv('api-url'),
        DYNAMODB_TABLE: this.table.tableName,
        MAIL_FROM: props.mailFrom,
        CLIENT_IP_HEADER,
        AWS_REGION: this.region,
      },
      secrets: {
        SERVICE_VAULT_API_KEY: ecs.Secret.fromSecretsManager(apiKey),
        SERVICE_VAULT_PIN: ecs.Secret.fromSsmParameter(tlsPin),
        WEBHOOK_SECRET: ecs.Secret.fromSecretsManager(webhookSecret),
        MEMBER_API_PEPPER: ecs.Secret.fromSecretsManager(pepper),
        ORIGIN_VERIFY_SECRET: ecs.Secret.fromSecretsManager(originSecret),
      },
      logging: ecs.LogDrivers.awsLogs({ logGroup, streamPrefix: 'member-api' }),
      stopTimeout: cdk.Duration.seconds(30),
      // The image has no shell: the binary probes its own /api/health.
      healthCheck: {
        command: ['CMD', '/member-api', 'healthcheck'],
        interval: cdk.Duration.seconds(30),
        timeout: cdk.Duration.seconds(5),
        retries: 3,
        startPeriod: cdk.Duration.seconds(15),
      },
    });

    // One Spot task, as the service vault: all state is in DynamoDB, so a
    // reclaimed task is replaced and nothing is lost. Deploys stop the old
    // task before starting the new one (min 0 / max 100); a request in the
    // gap fails and the page retries. The SRV record is what the VPC link
    // resolves (it needs the port).
    this.service = new ecs.FargateService(this, 'Service', {
      serviceName: prefix,
      cluster,
      taskDefinition: taskDef,
      desiredCount: 1,
      minHealthyPercent: 0,
      maxHealthyPercent: 100,
      circuitBreaker: { enable: true, rollback: true },
      capacityProviderStrategies: [{ capacityProvider: 'FARGATE_SPOT', weight: 1 }],
      assignPublicIp: true,
      securityGroups: [this.securityGroup],
      vpcSubnets: { subnets: vpc.publicSubnets },
      enableExecuteCommand: false,
      cloudMapOptions: {
        name: DISCOVERY_NAME,
        cloudMapNamespace: namespace,
        dnsRecordType: servicediscovery.DnsRecordType.SRV,
        dnsTtl: cdk.Duration.seconds(10),
        containerPort: API_PORT,
      },
    });

    // ---- HTTP API -----------------------------------------------------------
    const vpcLink = new apigwv2.VpcLink(this, 'VpcLink', {
      vpc,
      subnets: { subnets: vpc.publicSubnets },
      securityGroups: [linkSg],
      vpcLinkName: prefix,
    });
    this.api = new apigwv2.HttpApi(this, 'Api', {
      apiName: prefix,
      description: `VettID member API (${envName}), behind ${props.hostName} CloudFront`,
      defaultIntegration: new integrations.HttpServiceDiscoveryIntegration('MemberApi', this.service.cloudMapService!, { vpcLink }),
    });
    const stage = this.api.defaultStage!.node.defaultChild as apigwv2.CfnStage;
    stage.defaultRouteSettings = { throttlingRateLimit: 50, throttlingBurstLimit: 100 };
    // Request metadata only: no headers, cookies, bodies or addresses.
    const accessLogs = new logs.LogGroup(this, 'AccessLogs', {
      retention: logs.RetentionDays.ONE_MONTH,
      removalPolicy: cdk.RemovalPolicy.RETAIN,
    });
    stage.accessLogSettings = {
      destinationArn: accessLogs.logGroupArn,
      format: JSON.stringify({
        requestId: '$context.requestId',
        time: '$context.requestTime',
        method: '$context.httpMethod',
        path: '$context.path',
        status: '$context.status',
        bytes: '$context.responseLength',
        latencyMs: '$context.integrationLatency',
        error: '$context.integrationErrorMessage',
      }),
    };

    // ---- Site ---------------------------------------------------------------
    const zone = route53.HostedZone.fromHostedZoneAttributes(this, 'Zone', { hostedZoneId: props.hostedZoneId, zoneName: props.zoneName });
    this.site = new StaticSite(this, 'Site', {
      hostName: props.hostName,
      hostedZone: zone,
      sourceDir: SITE_DIR,
      notFoundPage: '404.html',
      apiOriginDomain: `${this.api.apiId}.execute-api.${this.region}.${this.urlSuffix}`,
      apiOriginHeaders: { [ORIGIN_HEADER]: originSecret.secretValue.unsafeUnwrap() },
      apiForwardHeaders: WEBHOOK_HEADERS,
    });

    // ---- Alarms ---------------------------------------------------------------
    let topic: sns.ITopic;
    if (props.alarmTopicArn) {
      topic = sns.Topic.fromTopicArn(this, 'AlarmTopic', props.alarmTopicArn);
    } else {
      const own = new sns.Topic(this, 'AlarmTopic', { displayName: `VettID member service alarms (${envName})` });
      own.addSubscription(new subs.EmailSubscription(props.alarmEmail!));
      topic = own;
    }
    for (const [name, description] of Object.entries(ALARMS)) {
      const metric = new logs.MetricFilter(this, `Alarm-${name}`, {
        logGroup,
        filterPattern: logs.FilterPattern.stringValue('$.alarm', '=', name),
        metricNamespace: `VettID/MemberService/${envName}`,
        metricName: name,
        metricValue: '1',
      }).metric({ statistic: 'Sum', period: cdk.Duration.minutes(5) });
      const alarm = new cloudwatch.Alarm(this, `Alarm-${name}-alarm`, {
        alarmName: `${prefix}-${name}`,
        alarmDescription: description,
        metric,
        threshold: 1,
        evaluationPeriods: 1,
        comparisonOperator: cloudwatch.ComparisonOperator.GREATER_THAN_OR_EQUAL_TO_THRESHOLD,
        treatMissingData: cloudwatch.TreatMissingData.NOT_BREACHING,
      });
      alarm.addAlarmAction(new cwactions.SnsAction(topic));
    }
    // Down: ECS reports the service's CPU every minute while a task runs
    // (without Container Insights too); no data for 10 minutes means no
    // task (a Spot reclamation is replaced in a minute or two).
    new cloudwatch.Alarm(this, 'Alarm-down', {
      alarmName: `${prefix}-down`,
      alarmDescription: 'No member API task has run for 10 minutes: the site cannot connect, sign in or show the roster',
      metric: this.service.metricCpuUtilization({ statistic: 'SampleCount', period: cdk.Duration.minutes(5) }),
      threshold: 1,
      evaluationPeriods: 2,
      comparisonOperator: cloudwatch.ComparisonOperator.LESS_THAN_THRESHOLD,
      treatMissingData: cloudwatch.TreatMissingData.BREACHING,
    }).addAlarmAction(new cwactions.SnsAction(topic));

    // ---- Outputs --------------------------------------------------------------
    const out = (id: string, value: string, description: string) => new cdk.CfnOutput(this, id, { value, description });
    out('SiteUrl', `https://${props.hostName}`, 'The site');
    out('WebhookUrl', `https://${props.hostName}/api/webhooks/service-vault`, 'The service vault\'s webhookUrl');
    out('TaskSecurityGroupId', this.securityGroup.securityGroupId, 'The member API task\'s security group');
    out('ServiceVaultApiKeySecretArn', apiKey.secretArn, 'The API key (add its SHA-256 to the service vault\'s api-key-hashes)');
    out('TableName', this.table.tableName, 'The DynamoDB table');
    out('LogGroupName', logGroup.logGroupName, 'The member API\'s log group');
    out('ServiceName', this.service.serviceName, 'The ECS service (force a new deployment to restart the task)');
    out('ClusterName', cluster.clusterName, 'The ECS cluster');
    out('AlarmTopicArn', topic.topicArn, 'Where the alarms go');
  }
}
