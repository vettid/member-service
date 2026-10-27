#!/usr/bin/env node
import * as cdk from 'aws-cdk-lib';
import { MemberServiceStack } from '../lib/member-service-stack';

// Parameters come from context (cdk.json, ~/.cdk.json or -c key=value):
//   envName (default prod), image (required, by digest), hostName,
//   hostedZoneId, zoneName, mailFrom (required), serviceVaultParams,
//   alarmTopicArn. See README "Deployment".
const app = new cdk.App();
const ctx = (key: string): string | undefined => {
  const v = app.node.tryGetContext(key);
  return v === undefined || v === '' ? undefined : String(v);
};
const required = (key: string): string => {
  const v = ctx(key);
  if (v === undefined) throw new Error(`context "${key}" is required (cdk synth -c ${key}=...)`);
  return v;
};

const envName = ctx('envName') ?? 'prod';
const stack = new MemberServiceStack(app, `MemberService-${envName}`, {
  envName,
  image: required('image'),
  hostName: required('hostName'),
  hostedZoneId: required('hostedZoneId'),
  zoneName: required('zoneName'),
  mailFrom: required('mailFrom'),
  serviceVaultParams: ctx('serviceVaultParams'),
  alarmTopicArn: ctx('alarmTopicArn'),
  env: { account: process.env.CDK_DEFAULT_ACCOUNT, region: process.env.CDK_DEFAULT_REGION ?? 'us-east-1' },
  description: `VettID member service (${envName})`,
});
cdk.Tags.of(stack).add('project', 'vettid');
cdk.Tags.of(stack).add('component', 'member-service');
cdk.Tags.of(stack).add('env', envName);
