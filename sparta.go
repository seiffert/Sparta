package sparta

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/voxelbrain/goptions"
)

func init() {
	rand.Seed(time.Now().Unix())
}

// ArbitraryJSONObject represents an untyped key-value object. CloudFormation resource representations
// are aggregated as []ArbitraryJSONObject before being marsharled to JSON
// for API operations.
type ArbitraryJSONObject map[string]interface{}

// AWS Principal ARNs from http://docs.aws.amazon.com/general/latest/gr/aws-arns-and-namespaces.html
const (
	// @enum AWSPrincipal
	S3Principal = "s3.amazonaws.com"
	// @enum AWSPrincipal
	SNSPrincipal = "sns.amazonaws.com"
	// @enum AWSPrincipal
	EC2Principal = "ec2.amazonaws.com"
	// @enum AWSPrincipal
	LambdaPrincipal = "lambda.amazonaws.com"
)

// AssumePolicyDocument defines common a IAM::Role PolicyDocument
// used as part of IAM::Role resource definitions
var AssumePolicyDocument = ArbitraryJSONObject{
	"Version": "2012-10-17",
	"Statement": []ArbitraryJSONObject{
		{
			"Effect": "Allow",
			"Principal": ArbitraryJSONObject{
				"Service": []string{LambdaPrincipal},
			},
			"Action": []string{"sts:AssumeRole"},
		},
		{
			"Effect": "Allow",
			"Principal": ArbitraryJSONObject{
				"Service": []string{EC2Principal},
			},
			"Action": []string{"sts:AssumeRole"},
		},
	},
}

// CommonIAMStatements defines common IAM::Role Policy Statement values for different AWS
// service types.  See http://docs.aws.amazon.com/general/latest/gr/aws-arns-and-namespaces.html#genref-aws-service-namespaces
// for names.
// http://docs.aws.amazon.com/lambda/latest/dg/monitoring-functions.html
// for more information.
var CommonIAMStatements = map[string]ArbitraryJSONObject{
	"cloudformation": ArbitraryJSONObject{
		"Action": []string{"logs:CreateLogGroup",
			"logs:CreateLogStream",
			"logs:PutLogEvents"},
		"Effect":   "Allow",
		"Resource": "arn:aws:logs:*:*:*",
	},
	"dynamodb": ArbitraryJSONObject{
		"Effect": "Allow",
		"Action": []string{"dynamodb:DescribeStream",
			"dynamodb:GetRecords",
			"dynamodb:GetShardIterator",
			"dynamodb:ListStreams",
		},
	},
	"kinesis": ArbitraryJSONObject{
		"Effect": "Allow",
		"Action": []string{"kinesis:GetRecords",
			"kinesis:GetShardIterator",
			"kinesis:DescribeStream",
			"kinesis:ListStreams",
		},
	},
}

// RE for sanitizing golang/JS layer
var reSanitize = regexp.MustCompile("[\\.\\-\\s]+")

// LambdaContext defines the AWS Lambda Context object provided by the AWS Lambda runtime.
// See http://docs.aws.amazon.com/lambda/latest/dg/nodejs-prog-model-context.html
// for more information on field values.  Note that the golang version doesn't functions
// defined on the Context object.
type LambdaContext struct {
	AWSRequestID       string `json:"awsRequestId"`
	InvokeID           string `json:"invokeid"`
	LogGroupName       string `json:"logGroupName"`
	LogStreamName      string `json:"logStreamName"`
	FunctionName       string `json:"functionName"`
	MemoryLimitInMB    string `json:"memoryLimitInMB"`
	FunctionVersion    string `json:"functionVersion"`
	InvokedFunctionARN string `json:"invokedFunctionArn"`
}

// Package private type to deserialize NodeJS proxied
// Lambda Event and Context information
type lambdaRequest struct {
	Event   json.RawMessage `json:"event"`
	Context LambdaContext   `json:"context"`
}

// LambdaFunction is the golang function signature required to support AWS Lambda execution.
// Standard HTTP response codes are used to signal AWS Lambda success/failure on the
// proxied context() object.  See http://docs.aws.amazon.com/lambda/latest/dg/nodejs-prog-model-context.html
// for more information.
//
// 	200 - 299       : Success
// 	<200 || >= 300  : Failure
//
// Content written to the ResponseWriter will be used as the
// response/Error value provided to AWS Lambda.
type LambdaFunction func(*json.RawMessage, *LambdaContext, *http.ResponseWriter, *logrus.Logger)

// LambdaFunctionOptions defines additional AWS Lambda execution params.  See the
// AWS Lambda FunctionConfiguration (http://docs.aws.amazon.com/lambda/latest/dg/API_FunctionConfiguration.html)
// docs for more information. Note that the "Runtime" field will be automatically set
// to "nodejs" (at least until golang is officially supported)
type LambdaFunctionOptions struct {
	// Additional function description
	Description string
	// Memory limit
	MemorySize int64
	// Timeout (seconds)
	Timeout int64
}

////////////////////////////////////////////////////////////////////////////////
// Types to handle permissions & push source configuration

// LambdaPermissionExporter defines an interface for polymorphic collection of
// Permission entries that support specialization for additional resource generation.
type LambdaPermissionExporter interface {
	// Export the permission object to a set of CloudFormation resources
	// in the provided resources param.  The targetLambdaFuncRef
	// interface represents the Fn::GetAtt "Arn" JSON value
	// of the parent Lambda target
	export(targetLambdaFuncRef interface{},
		resources ArbitraryJSONObject,
		logger *logrus.Logger) (string, error)
	// Return a `describe` compatible output for the given permission
	descriptionInfo() (string, string)
}

////////////////////////////////////////////////////////////////////////////////
// START - BasePermission
//

// BasePermission (http://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/aws-resource-lambda-permission.html)
// type for common AWS Lambda permission data.
type BasePermission struct {
	// The AWS account ID (without hyphens) of the source owner
	SourceAccount string `json:"SourceAccount,omitempty"`
	// The ARN of a resource that is invoking your function.
	SourceArn string `json:"SourceArn,omitempty"`
}

func (perm BasePermission) export(principal string,
	targetLambdaFuncRef interface{},
	resources ArbitraryJSONObject,
	logger *logrus.Logger) (string, error) {
	properties := ArbitraryJSONObject{
		"Action":       "lambda:InvokeFunction",
		"FunctionName": targetLambdaFuncRef,
		"Principal":    principal,
	}
	if "" != perm.SourceAccount {
		properties["SourceAccount"] = perm.SourceAccount
	}
	if "" != perm.SourceArn {
		properties["SourceArn"] = perm.SourceArn
	}

	primaryPermission := ArbitraryJSONObject{
		"Type":       "AWS::Lambda::Permission",
		"Properties": properties,
	}
	hash := sha1.New()
	hash.Write([]byte(principal))

	if "" != perm.SourceAccount {
		hash.Write([]byte(perm.SourceAccount))
	}
	if "" != perm.SourceArn {
		hash.Write([]byte(perm.SourceArn))
	}
	resourceName := fmt.Sprintf("LambdaPerm%s", hex.EncodeToString(hash.Sum(nil)))
	resources[resourceName] = primaryPermission
	return resourceName, nil
}

func (perm BasePermission) descriptionInfo(b *bytes.Buffer, logger *logrus.Logger) error {
	return errors.New("Describe not implemented")
}

//
// END - BasePermission
////////////////////////////////////////////////////////////////////////////////

////////////////////////////////////////////////////////////////////////////////
// START - LambdaPermission
//

// LambdaPermission type that creates a Lambda::Permission entry
// in the generated template, but does NOT automatically register the lambda
// with the BasePermission.SourceArn.  Typically used to register lambdas with
// externally managed event producers
type LambdaPermission struct {
	BasePermission
	// The entity for which you are granting permission to invoke the Lambda function
	Principal string
}

func (perm LambdaPermission) export(targetLambdaFuncRef interface{},
	resources ArbitraryJSONObject,
	logger *logrus.Logger) (string, error) {
	return perm.BasePermission.export(perm.Principal, targetLambdaFuncRef, resources, logger)
}

func (perm LambdaPermission) descriptionInfo() (string, string) {
	return "Source", perm.BasePermission.SourceArn
}

//
// END - LambdaPermission
////////////////////////////////////////////////////////////////////////////////

////////////////////////////////////////////////////////////////////////////////
// START - S3Permission
//

// S3Permission struct that imples the S3 BasePermission.SourceArn should be
// updated (via PutBucketNotificationConfiguration) to automatically push
// events to the owning Lambda.
// See http://docs.aws.amazon.com/lambda/latest/dg/intro-core-components.html#intro-core-components-event-sources
// for more information.
type S3Permission struct {
	BasePermission
	// S3 events to register for (eg: `[]string{"s3:ObjectCreated:*", "s3:ObjectRemoved:*"}`).
	Events []string `json:"Events,omitempty"`
	// S3.NotificationConfigurationFilter
	// to scope event forwarding.  See
	// 		http://docs.aws.amazon.com/AmazonS3/latest/dev/NotificationHowTo.html
	// for more information.
	Filter s3.NotificationConfigurationFilter `json:"Filter,omitempty"`
}

func (perm S3Permission) bucketName() string {
	bucketParts := strings.Split(perm.BasePermission.SourceArn, ":")
	return bucketParts[len(bucketParts)-1]
}

func (perm S3Permission) export(targetLambdaFuncRef interface{}, resources ArbitraryJSONObject, logger *logrus.Logger) (string, error) {

	targetLambdaResourceName, err := perm.BasePermission.export(S3Principal, targetLambdaFuncRef, resources, logger)
	if nil != err {
		return "", err
	}

	// Make sure the custom lambda that manages s3 notifications is provisioned.
	configuratorResName, err := ensureConfiguratorLambdaResource(S3Principal, perm.SourceArn, resources, logger)
	if nil != err {
		return "", err
	}
	permissionData := ArbitraryJSONObject{
		"Events": perm.Events,
	}
	if nil != perm.Filter.Key {
		permissionData["Filter"] = perm.Filter
	}

	// Add a custom resource invocation for this configuration
	//////////////////////////////////////////////////////////////////////////////
	// And finally the custom resource forwarder

	customResourceInvoker := ArbitraryJSONObject{
		"Type":    "AWS::CloudFormation::CustomResource",
		"Version": "1.0",
		"Properties": ArbitraryJSONObject{
			"ServiceToken": ArbitraryJSONObject{
				"Fn::GetAtt": []string{configuratorResName, "Arn"},
			},
			"Permission": permissionData,
			// Use the LambdaTarget value in the JS custom resoruce
			// handler to create the ID used to manage S3 notifications
			"LambdaTarget": targetLambdaFuncRef,
			"Bucket":       perm.bucketName(),
		},
		"DependsOn": []string{targetLambdaResourceName, configuratorResName},
	}
	// Save it
	resourceInvokerName := CloudFormationResourceName(fmt.Sprintf("ConfigS3%s", targetLambdaResourceName))
	resources[resourceInvokerName] = customResourceInvoker
	return "", nil
}

func (perm S3Permission) descriptionInfo() (string, string) {
	return perm.BasePermission.SourceArn, fmt.Sprintf("%s", perm.Events)
}

//
// END - S3Permission
///////////////////////////////////////////////////////////////////////////////////

////////////////////////////////////////////////////////////////////////////////
// SNSPermission - START

// SNSPermission struct that imples the S3 BasePermission.SourceArn should be
// updated (via PutBucketNotificationConfiguration) to automatically push
// events to the parent Lambda.
// See http://docs.aws.amazon.com/lambda/latest/dg/intro-core-components.html#intro-core-components-event-sources
// for more information.
type SNSPermission struct {
	BasePermission
}

func (perm SNSPermission) topicName() string {
	topicParts := strings.Split(perm.BasePermission.SourceArn, ":")
	return topicParts[len(topicParts)-1]
}

func (perm SNSPermission) export(targetLambdaFuncRef interface{}, resources ArbitraryJSONObject, logger *logrus.Logger) (string, error) {

	targetLambdaResourceName, err := perm.BasePermission.export(SNSPrincipal, targetLambdaFuncRef, resources, logger)
	if nil != err {
		return "", err
	}

	// Make sure the custom lambda that manages SNS notifications is provisioned.
	configuratorResName, err := ensureConfiguratorLambdaResource(SNSPrincipal, perm.SourceArn, resources, logger)
	if nil != err {
		return "", err
	}

	// Add a custom resource invocation for this configuration
	//////////////////////////////////////////////////////////////////////////////
	// And the custom resource forwarder
	customResourceSubscriber := ArbitraryJSONObject{
		"Type":    "AWS::CloudFormation::CustomResource",
		"Version": "1.0",
		"Properties": ArbitraryJSONObject{
			"ServiceToken": ArbitraryJSONObject{
				"Fn::GetAtt": []string{configuratorResName, "Arn"},
			},
			"Mode":     "Subscribe",
			"TopicArn": perm.BasePermission.SourceArn,
			// Use the LambdaTarget value in the JS custom resoruce
			// handler to create the ID used to manage S3 notifications
			"LambdaTarget": targetLambdaFuncRef,
		},
		"DependsOn": []string{targetLambdaResourceName, configuratorResName},
	}
	// Save it
	subscriberResourceName := CloudFormationResourceName(fmt.Sprintf("SubscriberSNS%s", targetLambdaResourceName))
	resources[subscriberResourceName] = customResourceSubscriber

	//////////////////////////////////////////////////////////////////////////////
	// And the custom resource unsubscriber
	customResourceUnsubscriber := ArbitraryJSONObject{
		"Type":    "AWS::CloudFormation::CustomResource",
		"Version": "1.0",
		"Properties": ArbitraryJSONObject{
			"ServiceToken": ArbitraryJSONObject{
				"Fn::GetAtt": []string{configuratorResName, "Arn"},
			},
			"Mode": "Unsubscribe",
			"SubscriptionArn": ArbitraryJSONObject{
				"Fn::GetAtt": []string{subscriberResourceName, "SubscriptionArn"},
			},
			"TopicArn": perm.BasePermission.SourceArn,
			// Use the LambdaTarget value in the JS custom resoruce
			// handler to create the ID used to manage S3 notifications
			"LambdaTarget": targetLambdaFuncRef,
		},
		"DependsOn": []string{subscriberResourceName},
	}
	// Save it
	unsubscriberResourceName := CloudFormationResourceName(fmt.Sprintf("UnsubscriberSNS%s", targetLambdaResourceName))
	resources[unsubscriberResourceName] = customResourceUnsubscriber

	return "", nil
}

func (perm SNSPermission) descriptionInfo() (string, string) {
	return perm.BasePermission.SourceArn, ""
}

////////////////////////////////////////////////////////////////////////////////
// START - IAM
//

// IAMRolePrivilege struct stores data necessary to create an IAM Policy Document
// as part of the inline IAM::Role resource definition.  See
// http://docs.aws.amazon.com/IAM/latest/UserGuide/access_policies.html
// for more information
type IAMRolePrivilege struct {
	// What actions you will allow.
	// Each AWS service has its own set of actions.
	// For example, you might allow a user to use the Amazon S3 ListBucket action,
	// which returns information about the items in a bucket.
	// Any actions that you don't explicitly allow are denied.
	Actions []string
	// Which resources you allow the action on. For example, what specific Amazon
	// S3 buckets will you allow the user to perform the ListBucket action on?
	// Users cannot access any resources that you have not explicitly granted
	// permissions to.
	Resource string
}

// IAMRoleDefinition stores a slice of IAMRolePrivilege values
// to "Allow" for the given IAM::Role.
// Note that the CommonIAMStatements will be automatically included and do
// not need to be multiply specified.
type IAMRoleDefinition struct {
	// Slice of IAMRolePrivilege entries
	Privileges []IAMRolePrivilege
}

// Returns an IAM::Role policy entry for this definition
func (roleDefinition *IAMRoleDefinition) rolePolicy(eventSourceMappings []*lambda.CreateEventSourceMappingInput, logger *logrus.Logger) ArbitraryJSONObject {
	statements := []ArbitraryJSONObject{CommonIAMStatements["cloudformation"]}
	for _, eachPrivilege := range roleDefinition.Privileges {
		statements = append(statements, ArbitraryJSONObject{
			"Effect":   "Allow",
			"Action":   eachPrivilege.Actions,
			"Resource": eachPrivilege.Resource,
		})
	}

	// // http://docs.aws.amazon.com/general/latest/gr/aws-arns-and-namespaces.html
	for _, eachEventSourceMapping := range eventSourceMappings {
		arnParts := strings.Split(*eachEventSourceMapping.EventSourceArn, ":")
		// 3rd slot is service scope
		if len(arnParts) >= 2 {
			awsService := arnParts[2]
			logger.Debug("Looking up common IAM privileges for EventSource: ", awsService)
			serviceStatements, exists := CommonIAMStatements[awsService]
			if exists {
				statements = append(statements, serviceStatements)
				statements[len(statements)-1]["Resource"] = *eachEventSourceMapping.EventSourceArn
			}
		}
	}
	iamPolicy := ArbitraryJSONObject{"Type": "AWS::IAM::Role",
		"Properties": ArbitraryJSONObject{
			"AssumeRolePolicyDocument": AssumePolicyDocument,
			"Policies": []ArbitraryJSONObject{
				{
					"PolicyName": CloudFormationResourceName("LambdaPolicy"),
					"PolicyDocument": ArbitraryJSONObject{
						"Version":   "2012-10-17",
						"Statement": statements,
					},
				},
			},
		},
	}
	return iamPolicy
}

// Returns the stable logical name for this IAMRoleDefinition
func (roleDefinition *IAMRoleDefinition) logicalName() string {
	hash := sha1.New()
	hash.Write([]byte(fmt.Sprintf("%s", roleDefinition.Privileges)))
	return fmt.Sprintf("IAMRole%s", hex.EncodeToString(hash.Sum(nil)))
}

//
// END - IAM
////////////////////////////////////////////////////////////////////////////////

////////////////////////////////////////////////////////////////////////////////
// START - LambdaAWSInfo
//

// LambdaAWSInfo stores all data necessary to provision a golang-based AWS Lambda function.
type LambdaAWSInfo struct {
	// internal function name, determined by reflection
	lambdaFnName string
	// pointer to lambda function
	lambdaFn LambdaFunction
	// Role name (NOT ARN) to use during AWS Lambda Execution.  See
	// the FunctionConfiguration (http://docs.aws.amazon.com/lambda/latest/dg/API_FunctionConfiguration.html)
	// docs for more info.
	// Note that either `RoleName` or `RoleDefinition` must be supplied
	RoleName string
	// IAM Role Definition if the stack should implicitly create an IAM role for
	// lambda execution. Note that either `RoleName` or `RoleDefinition` must be supplied
	RoleDefinition *IAMRoleDefinition
	// Additional exeuction options
	Options *LambdaFunctionOptions
	// Permissions to enable push-based Lambda execution.  See the
	// Permission Model docs (http://docs.aws.amazon.com/lambda/latest/dg/intro-permission-model.html)
	// for more information.
	Permissions []LambdaPermissionExporter
	// EventSource mappings to enable for pull-based Lambda execution.  See the
	// Event Source docs (http://docs.aws.amazon.com/lambda/latest/dg/intro-core-components.html)
	// for more information
	EventSourceMappings []*lambda.CreateEventSourceMappingInput
}

// Returns a JavaScript compatible function name for the golang function name.  This
// value will be used as the URL path component for the HTTP proxying layer.
func (info *LambdaAWSInfo) jsHandlerName() string {
	return sanitizedName(info.lambdaFnName)
}

// Marshal this object into 1 or more CloudFormation resource definitions that are accumulated
// in the resources map
func (info *LambdaAWSInfo) export(S3Bucket string,
	S3Key string,
	roleNameMap map[string]interface{},
	resources ArbitraryJSONObject,
	logger *logrus.Logger) error {

	// If we have RoleName, then get the ARN, otherwise get the Ref
	var dependsOn []string

	iamRoleArnName := info.RoleName
	// If there is no user supplied role, that means that the associated
	// IAMRoleDefinition name has been created and this resource needs to
	// depend on that existing.
	if iamRoleArnName == "" {
		iamRoleArnName = info.RoleDefinition.logicalName()
		dependsOn = append(dependsOn, iamRoleArnName)
	}

	// Create the primary resource
	primaryResource := ArbitraryJSONObject{
		"Type": "AWS::Lambda::Function",
		"Properties": ArbitraryJSONObject{
			"Code": ArbitraryJSONObject{
				"S3Bucket": S3Bucket,
				"S3Key":    S3Key,
			},
			"Description": info.Options.Description,
			"Handler":     fmt.Sprintf("index.%s", info.jsHandlerName()),
			"MemorySize":  info.Options.MemorySize,
			"Role":        roleNameMap[iamRoleArnName],
			"Runtime":     "nodejs",
			"Timeout":     info.Options.Timeout,
		},
		"DependsOn": dependsOn,
	}

	// Get the resource name we're going to use s.t. we can tie it to the rest of the
	// lambda definition
	hash := sha1.New()
	hash.Write([]byte(info.lambdaFnName))
	resourceName := fmt.Sprintf("Lambda%s", hex.EncodeToString(hash.Sum(nil)))
	resources[resourceName] = primaryResource

	// Create the lambda Ref in case we need a permission or event mapping
	functionAttr := ArbitraryJSONObject{
		"Fn::GetAtt": []string{resourceName, "Arn"},
	}

	// Permissions
	for _, eachPermission := range info.Permissions {
		_, err := eachPermission.export(functionAttr, resources, logger)
		if nil != err {
			return err
		}
	}

	// Event Source Mappings
	for _, eachEventSourceMapping := range info.EventSourceMappings {
		properties := ArbitraryJSONObject{
			"EventSourceArn":   eachEventSourceMapping.EventSourceArn,
			"FunctionName":     functionAttr,
			"StartingPosition": eachEventSourceMapping.StartingPosition,
			"BatchSize":        eachEventSourceMapping.BatchSize,
		}
		if nil != eachEventSourceMapping.Enabled {
			properties["Enabled"] = *eachEventSourceMapping.Enabled
		}

		primaryEventSourceMapping := ArbitraryJSONObject{
			"Type":       "AWS::Lambda::EventSourceMapping",
			"Properties": properties,
		}
		hash := sha1.New()
		hash.Write([]byte(*eachEventSourceMapping.EventSourceArn))
		binary.Write(hash, binary.LittleEndian, *eachEventSourceMapping.BatchSize)
		hash.Write([]byte(*eachEventSourceMapping.StartingPosition))
		resourceName := fmt.Sprintf("LambdaES%s", hex.EncodeToString(hash.Sum(nil)))
		resources[resourceName] = primaryEventSourceMapping
	}
	return nil
}

//
// END - LambdaAWSInfo
////////////////////////////////////////////////////////////////////////////////

////////////////////////////////////////////////////////////////////////////////
// Private
//
// Sanitize the provided input by replacing illegal characters with underscores
func sanitizedName(input string) string {
	return reSanitize.ReplaceAllString(input, "_")
}

// Returns an AWS Session (https://github.com/aws/aws-sdk-go/wiki/Getting-Started-Configuration)
// object that attaches a debug level handler to all AWS requests from services
// sharing the session value.
func awsSession(logger *logrus.Logger) *session.Session {
	sess := session.New()
	sess.Handlers.Send.PushFront(func(r *request.Request) {
		logger.WithFields(logrus.Fields{
			"Service":   r.ClientInfo.ServiceName,
			"Operation": r.Operation.Name,
			"Method":    r.Operation.HTTPMethod,
			"Path":      r.Operation.HTTPPath,
			"Payload":   r.Params,
		}).Debug("AWS Request")
	})
	return sess
}

// CloudFormationResourceName returns a name suitable as a logical
// CloudFormation resource value.  See http://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/resources-section-structure.html
// for more information.  The `prefix` value should provide a hint as to the
// resource type (eg, `SNSConfigurator`, `ImageTranscoder`).  Note that the returned
// name is not content-addressable.
func CloudFormationResourceName(prefix string) string {
	randValue := rand.Int63()
	hash := sha1.New()
	hash.Write([]byte(prefix))
	hash.Write([]byte(strconv.FormatInt(randValue, 10)))
	return fmt.Sprintf("%s%s", prefix, hex.EncodeToString(hash.Sum(nil)))
}

////////////////////////////////////////////////////////////////////////////////
// Public
////////////////////////////////////////////////////////////////////////////////

// NewLambda returns a LambdaAWSInfo value that can be provisioned via CloudFormation. The
// roleNameOrIAMRoleDefinition must either be a `string` or `IAMRoleDefinition`
// type
func NewLambda(roleNameOrIAMRoleDefinition interface{}, fn LambdaFunction, lambdaOptions *LambdaFunctionOptions) *LambdaAWSInfo {
	if nil == lambdaOptions {
		lambdaOptions = &LambdaFunctionOptions{"", 128, 3}
	}
	lambdaPtr := runtime.FuncForPC(reflect.ValueOf(fn).Pointer())
	lambda := &LambdaAWSInfo{
		lambdaFnName:        lambdaPtr.Name(),
		lambdaFn:            fn,
		Options:             lambdaOptions,
		Permissions:         make([]LambdaPermissionExporter, 0),
		EventSourceMappings: make([]*lambda.CreateEventSourceMappingInput, 0),
	}

	switch v := roleNameOrIAMRoleDefinition.(type) {
	case string:
		lambda.RoleName = roleNameOrIAMRoleDefinition.(string)
	case IAMRoleDefinition:
		definition := roleNameOrIAMRoleDefinition.(IAMRoleDefinition)
		lambda.RoleDefinition = &definition
	default:
		panic(fmt.Sprintf("Unsupported IAM Role type: %s", v))
	}

	// Defaults
	if lambda.Options.MemorySize <= 0 {
		lambda.Options.MemorySize = 128
	}
	if lambda.Options.Timeout <= 0 {
		lambda.Options.Timeout = 3
	}
	return lambda
}

// NewLogger returns a new logrus.Logger instance. It is the caller's responsibility
// to set the formatter if needed.
func NewLogger(level string) (*logrus.Logger, error) {
	logger := logrus.New()
	logLevel, err := logrus.ParseLevel(level)
	if err != nil {
		return nil, err
	}
	logger.Level = logLevel
	return logger, nil
}

// Main defines the primary handler for transforming an application into a Sparta package.  The
// serviceName is used to uniquely identify your service within a region and will
// be used for subsequent updates.  For provisioning, ensure that you've
// properly configured AWS credentials for the golang SDK.
// See http://docs.aws.amazon.com/sdk-for-go/api/aws/defaults.html#DefaultChainCredentials-constant
// for more information.
func Main(serviceName string, serviceDescription string, lambdaAWSInfos []*LambdaAWSInfo) error {

	// We need to be able to provision an IAM role that has capabilities to
	// manage the other sources.  That'll give us the role arn to use in the custom
	// resource execution.
	options := struct {
		LogLevel string        `goptions:"-l, --level, description='Log level [panic, fatal, error, warn, info, debug]'"`
		Help     goptions.Help `goptions:"-h, --help, description='Show this help'"`

		Verb      goptions.Verbs
		Provision struct {
			S3Bucket string `goptions:"-b,--s3Bucket, description='S3 Bucket to use for Lambda source', obligatory"`
		} `goptions:"provision"`
		Delete struct {
		} `goptions:"delete"`
		Execute struct {
			Port            int `goptions:"-p,--port, description='Alternative port for HTTP binding (default=9999)'"`
			SignalParentPID int `goptions:"-s,--signal, description='Process ID to signal with SIGUSR2 once ready'"`
		} `goptions:"execute"`
		Describe struct {
			OutputFile string `goptions:"-o,--out, description='Output file for HTML description', obligatory"`
		} `goptions:"describe"`
		Explore struct {
		} `goptions:"explore"`
	}{ // Default values goes here
		LogLevel: "info",
	}
	goptions.ParseAndFail(&options)
	logger, err := NewLogger(options.LogLevel)
	if err != nil {
		goptions.PrintHelp()
		os.Exit(1)
	}
	switch options.Verb {
	case "provision":
		logger.Formatter = new(logrus.TextFormatter)
		return Provision(serviceName, serviceDescription, lambdaAWSInfos, options.Provision.S3Bucket, logger)
	case "execute":
		logger.Formatter = new(logrus.JSONFormatter)
		return Execute(lambdaAWSInfos, options.Execute.Port, options.Execute.SignalParentPID, logger)
	case "delete":
		logger.Formatter = new(logrus.TextFormatter)
		return Delete(serviceName, logger)
	case "explore":
		logger.Formatter = new(logrus.TextFormatter)
		return Explore(serviceName, logger)
	case "describe":
		logger.Formatter = new(logrus.TextFormatter)
		fileWriter, err := os.Create(options.Describe.OutputFile)
		if err != nil {
			errMsg := fmt.Sprintf("Failed to open %s output. Error: %s", options.Describe.OutputFile, err)
			return errors.New(errMsg)
		}
		defer fileWriter.Close()
		return Describe(serviceName, serviceDescription, lambdaAWSInfos, fileWriter, logger)
	default:
		goptions.PrintHelp()
		return errors.New("Unsupported subcommand: " + string(options.Verb))
	}
}
